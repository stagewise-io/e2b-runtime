//go:build linux

package sandbox

import (
	"context"
	"errors"
	"sync"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// SnapshotError separates an artifact failure from its write-cleanup outcome.
// A nil WaitCleanup result proves that no writer can consume more disk. An
// ordinary error, a nil callback, or a failed wait provides no such proof.
// Cleanup must not delete artifacts already handed to the template cache.
type SnapshotError struct {
	Err         error
	WaitCleanup func(context.Context) error
}

func (e *SnapshotError) Error() string { return e.Err.Error() }
func (e *SnapshotError) Unwrap() error { return e.Err }

// snapshotFailure records the outcome of synchronous rollback. The operation
// error remains available through errors.Is/As, independently of cleanup.
func snapshotFailure(err, cleanupErr error) error {
	return &SnapshotError{
		Err:         errors.Join(err, cleanupErr),
		WaitCleanup: func(context.Context) error { return cleanupErr },
	}
}

// snapshotCleanupError is only used after joining a producer. A producer whose
// partial-file cleanup is not explicitly confirmed remains fail-closed.
func snapshotCleanupError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var failure *SnapshotError
	if errors.As(err, &failure) && failure.WaitCleanup != nil {
		return failure.WaitCleanup(context.WithoutCancel(ctx))
	}

	return err
}

// snapshotWriters joins producer teardown, not just publication of its diff
// promise. A promise can fail before fault-path copies drain or a cache closes.
// All starts happen during Pause, before the Snapshot is handed to a caller.
type snapshotWriters struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	err  error
	done *utils.SetOnce[struct{}]
}

func (w *snapshotWriters) start(run func() error) {
	w.wg.Go(func() {
		err := run()
		w.mu.Lock()
		w.err = errors.Join(w.err, err)
		w.mu.Unlock()
	})
}

func (w *snapshotWriters) seal() {
	w.done = utils.NewSetOnce[struct{}]()
	go func() {
		w.wg.Wait()
		w.mu.Lock()
		err := w.err
		w.mu.Unlock()
		_ = w.done.SetResult(struct{}{}, err)
	}()
}

func (w *snapshotWriters) wait(ctx context.Context) error {
	_, err := w.done.WaitWithContext(ctx)

	return err
}

// WaitForWrites joins every deferred producer, even if another producer failed.
// Artifact errors are not cleanup errors. The memfd copy publishes its result
// only after it stops writing; its cached bytes remain measured by statfs.
func (s *Snapshot) WaitForWrites(ctx context.Context) error {
	var err error
	if s.cleanup != nil && s.cleanup.snapshotWriters.done != nil {
		err = s.cleanup.snapshotWriters.wait(ctx)
	}
	if s.MemorySnapshot.Diff != nil {
		_, _ = s.MemorySnapshot.Diff.CachePath(ctx)
	}
	if s.RootfsDiff != nil {
		_, _ = s.RootfsDiff.CachePath(ctx)
	}
	if ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}

	return err
}
