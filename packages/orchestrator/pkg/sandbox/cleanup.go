//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type Cleanup struct {
	cleanup         []func(ctx context.Context) error
	priorityCleanup []func(ctx context.Context) error
	error           error
	once            sync.Once

	resourceLease       *resourcecontroller.Lease
	resourceWork        int
	resourceErrors      error
	resourceCleanupDone bool

	hasRun atomic.Bool
	mu     sync.Mutex
}

const lateCleanupLogMessage = "cleanup callback ran after cleanup completed"

func runLateCleanup(ctx context.Context, f func(context.Context) error, priority bool) error {
	runCtx := context.WithoutCancel(ctx)
	start := time.Now()
	err := f(runCtx)
	fields := []zap.Field{
		zap.Bool("priority", priority),
		zap.Duration("duration", time.Since(start)),
	}
	if err != nil {
		logger.L().Error(runCtx, lateCleanupLogMessage, append(fields, zap.Error(err))...)

		return err
	}

	logger.L().Info(runCtx, lateCleanupLogMessage, fields...)

	return nil
}

// preserveConstructionCause retains the reason a failed constructor was canceled,
// including when a dependency returned only the generic context error.
func preserveConstructionCause(ctx context.Context, err error) error {
	if err == nil || !errors.Is(err, ctx.Err()) {
		return err
	}
	//nolint:errorlint // A wrapped cancellation can itself carry the diagnosis; only ctx.Err() is generic.
	if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() && !errors.Is(err, cause) {
		return errors.Join(err, cause)
	}

	return err
}

func NewCleanup() *Cleanup {
	return &Cleanup{}
}

func (c *Cleanup) AddNoContext(ctx context.Context, f func() error) {
	c.Add(ctx, func(_ context.Context) error { return f() })
}

func (c *Cleanup) Add(ctx context.Context, f func(ctx context.Context) error) {
	// Keep the state check and registration under the same lock as Run. Without
	// this, Add can observe false, lose the lock to Run, then append after Run has
	// already drained the list.
	c.mu.Lock()
	if !c.hasRun.Load() {
		c.cleanup = append(c.cleanup, f)
		c.mu.Unlock()

		return
	}
	complete := c.trackResourceWorkLocked()
	c.mu.Unlock()

	complete(runLateCleanup(ctx, f, false))
}

func (c *Cleanup) AddPriority(ctx context.Context, f func(ctx context.Context) error) {
	c.mu.Lock()
	if !c.hasRun.Load() {
		c.priorityCleanup = append(c.priorityCleanup, f)
		c.mu.Unlock()

		return
	}
	complete := c.trackResourceWorkLocked()
	c.mu.Unlock()

	complete(runLateCleanup(ctx, f, true))
}

func (c *Cleanup) Run(ctx context.Context) error {
	c.once.Do(func() {
		c.run(context.WithoutCancel(ctx))
	})

	return c.error
}

// cleanupIfNotRegistered covers errors before a resource's normal cleanup callback is registered.
func (c *Cleanup) cleanupIfNotRegistered(ctx context.Context, result *error, registered *bool, f func(context.Context) error) {
	if *result != nil && !*registered {
		err := f(context.WithoutCancel(ctx))
		c.recordResourceError(err)
		*result = errors.Join(*result, err)
	}
}

func (c *Cleanup) run(ctx context.Context) {
	c.hasRun.Store(true)

	c.mu.Lock()
	priorityCleanup := c.priorityCleanup
	normalCleanup := c.cleanup
	c.mu.Unlock()

	var errs []error

	for _, f := range slices.Backward(priorityCleanup) {
		err := f(ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	for _, f := range slices.Backward(normalCleanup) {
		err := f(ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	c.error = errors.Join(errs...)
	c.mu.Lock()
	c.resourceErrors = errors.Join(c.resourceErrors, c.error)
	c.resourceCleanupDone = true
	c.releaseResourcesLocked()
	c.mu.Unlock()
}

// bindResources runs before construction launches asynchronous work.
func (c *Cleanup) bindResources(lease *resourcecontroller.Lease) {
	c.resourceLease = lease
}

// trackResourceWork keeps admission charged across late registration and async
// teardown. Register before starting the worker; its completion is idempotent.
func (c *Cleanup) trackResourceWork() func(error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.trackResourceWorkLocked()
}

// trackResourceWorkLocked registers before Run can release the lease.
func (c *Cleanup) trackResourceWorkLocked() func(error) {
	if c.resourceLease == nil {
		return func(error) {}
	}
	c.resourceWork++
	var once sync.Once

	return func(err error) {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.resourceErrors = errors.Join(c.resourceErrors, err)
			c.resourceWork--
			c.releaseResourcesLocked()
		})
	}
}

func (c *Cleanup) recordResourceError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resourceErrors = errors.Join(c.resourceErrors, err)
}

func (c *Cleanup) releaseResourcesLocked() {
	if c.resourceLease != nil && c.resourceCleanupDone && c.resourceWork == 0 {
		c.resourceLease.Release(c.resourceErrors)
	}
}

func cleanupFiles(config cfg.BuilderConfig, files *storage.SandboxFiles) func(context.Context) error {
	return func(context.Context) error {
		var errs []error

		for _, p := range []string{
			files.SandboxFirecrackerSocketPath(),
			files.SandboxUffdSocketPath(),
			files.SandboxCacheRootfsLinkPath(config.StorageConfig),
		} {
			err := os.RemoveAll(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to delete '%s': %w", p, err))
			}
		}

		if len(errs) == 0 {
			return nil
		}

		return fmt.Errorf("failed to cleanup files: %w", errors.Join(errs...))
	}
}
