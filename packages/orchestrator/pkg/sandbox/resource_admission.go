//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

type resourceLeaseKey struct{}

// WithResourceLease passes a pre-admitted start through reboot or resume. The
// constructor consumes the value so execution contexts cannot reuse the lease.
// The caller must defer ReleaseUnclaimed for errors before factory ownership.
func WithResourceLease(ctx context.Context, lease *resourcecontroller.Lease) context.Context {
	return context.WithValue(ctx, resourceLeaseKey{}, lease)
}

func takeResourceLease(ctx context.Context) (context.Context, *resourcecontroller.Lease) {
	lease, _ := ctx.Value(resourceLeaseKey{}).(*resourcecontroller.Lease)
	if lease == nil {
		return ctx, nil
	}

	return context.WithValue(ctx, resourceLeaseKey{}, (*resourcecontroller.Lease)(nil)), lease
}

func (f *Factory) resourceEnforced() bool {
	return f.config.ResourceAdmission.Enabled || f.Admission.Enabled()
}

// prepareResourceAdmission runs before cgroup, network, memory or rootfs setup.
func (f *Factory) prepareResourceAdmission(ctx context.Context, t template.Template, config *Config, directPath, lifecycle string, cleanup *Cleanup, incoming *resourcecontroller.Lease) (*resourcecontroller.Lease, error) {
	if !f.resourceEnforced() {
		return nil, nil
	}
	if !f.Admission.Enabled() {
		return nil, errors.New("enabled resource admission requires a shared controller")
	}
	lease := incoming
	if lease != nil {
		path := ""
		if directPath != "" {
			path = filepath.Dir(directPath)
		}
		if err := lease.ValidateStart(f.Admission, config.Vcpu, path); err != nil {
			return nil, err
		}
		if err := lease.Claim(lifecycle); err != nil {
			return nil, err
		}
		cleanup.bindResources(lease)
	}
	rootfs, err := t.Rootfs()
	if err != nil {
		return nil, fmt.Errorf("resource admission rootfs: %w", err)
	}
	size, err := rootfs.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("resource admission rootfs size: %w", err)
	}
	if size <= 0 || config.Vcpu <= 0 {
		return nil, errors.New("resource admission requires positive CPU and rootfs size")
	}
	if lease == nil {
		resources := resourceadmission.Resources{CPUs: uint64(config.Vcpu), DiskBytes: uint64(size)}
		if directPath == "" {
			lease, err = f.Admission.Acquire(resources)
		} else {
			lease, err = f.Admission.AcquireAt(resources, filepath.Dir(directPath))
		}
		if err != nil {
			return nil, err
		}
		if err := lease.Claim(lifecycle); err != nil {
			lease.ReleaseUnclaimed()

			return nil, err
		}
		cleanup.bindResources(lease)
	}
	if err := lease.Grow(uint64(size)); err != nil {
		return nil, err
	}

	return lease, nil
}

func resourceStarted(lease *resourcecontroller.Lease) {
	if lease != nil {
		lease.Started()
	}
}
