//go:build linux

package sandbox

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func admissionController(t *testing.T) *resourcecontroller.Controller {
	t.Helper()
	c, err := resourcecontroller.New(resourcecontroller.Options{
		Policy:   resourceadmission.Policy{Enabled: true, CPUPercent: 100, DiskPercent: 80, HeadroomBytes: 100},
		CPUCount: 2,
		Paths:    map[string]string{resourcecontroller.Rootfs: "/rootfs", resourcecontroller.Build: "/build", resourcecontroller.Template: "/template"},
		Sample: func(path string) (resourcecontroller.Sample, error) {
			id := "root"
			if path == "/build" {
				id = "build"
			}

			return resourcecontroller.Sample{ID: id, TotalBytes: 1000, AvailableBytes: 1000}, nil
		},
	})
	require.NoError(t, err)

	return c
}

func boundCleanup(t *testing.T, c *resourcecontroller.Controller) *Cleanup {
	t.Helper()
	lease, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 500})
	require.NoError(t, err)
	require.NoError(t, lease.Claim("lifecycle"))
	cleanup := NewCleanup()
	cleanup.bindResources(lease)

	return cleanup
}

func TestResourceCleanupWaitsForLateWorker(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed cleanup"}[fail], func(t *testing.T) {
			t.Parallel()
			c := admissionController(t)
			cleanup := boundCleanup(t, c)
			complete := cleanup.trackResourceWork()
			require.NoError(t, cleanup.Run(t.Context()))
			require.Equal(t, uint64(2), c.Report().CPUCommitted)
			_, err := c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
			require.Error(t, err)
			called := false
			cleanup.Add(t.Context(), func(context.Context) error {
				called = true
				require.Equal(t, uint64(2), c.Report().CPUCommitted)
				if fail {
					return errors.New("disk still in use")
				}

				return nil
			})
			require.True(t, called)
			complete(nil)
			complete(errors.New("duplicate completion must be ignored"))
			require.NoError(t, cleanup.Run(t.Context()))
			report := c.Report()
			require.Equal(t, fail, report.Uncertain)
			if fail {
				require.Equal(t, uint64(2), report.CPUCommitted)
			} else {
				require.Zero(t, report.CPUCommitted)
			}
		})
	}
}

func TestResourceCleanupDoesNotReleaseInsideCallback(t *testing.T) {
	t.Parallel()
	c := admissionController(t)
	cleanup := boundCleanup(t, c)
	entered, finish := make(chan struct{}), make(chan struct{})
	cleanup.Add(t.Context(), func(context.Context) error {
		close(entered)
		<-finish

		return nil
	})
	done := make(chan error, 1)
	go func() { done <- cleanup.Run(t.Context()) }()
	<-entered
	require.Equal(t, uint64(2), c.Report().CPUCommitted)
	close(finish)
	require.NoError(t, <-done)
	require.Zero(t, c.Report().CPUCommitted)
}

func TestResourceCleanupTracksConcurrentLateCallbacks(t *testing.T) {
	t.Parallel()
	for _, priority := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			name := map[bool]string{false: "normal", true: "priority"}[priority] + "/" + map[bool]string{false: "success", true: "failure"}[fail]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					c := admissionController(t)
					cleanup := boundCleanup(t, c)
					runEntered, runFinish := make(chan struct{}), make(chan struct{})
					cleanup.Add(t.Context(), func(context.Context) error {
						close(runEntered)
						<-runFinish

						return nil
					})
					runDone := make(chan error, 1)
					go func() { runDone <- cleanup.Run(t.Context()) }()
					<-runEntered
					lateEntered, lateFinish, lateDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
					add := cleanup.Add
					if priority {
						add = cleanup.AddPriority
					}
					go func() {
						defer close(lateDone)
						add(t.Context(), func(context.Context) error {
							close(lateEntered)
							<-lateFinish
							if fail {
								return errors.New("late cleanup still owns disk")
							}

							return nil
						})
					}()
					<-lateEntered
					close(runFinish)
					synctest.Wait()
					during := c.Report()
					close(lateFinish)
					<-lateDone
					require.NoError(t, <-runDone)
					require.Equal(t, uint64(2), during.CPUCommitted, "Run must not release a lease while a late callback runs")
					after := c.Report()
					require.Equal(t, fail, after.Uncertain)
					if fail {
						require.Equal(t, uint64(2), after.CPUCommitted)
					} else {
						require.Zero(t, after.CPUCommitted)
					}
				})
			})
		}
	}
}

type resourceAdmissionTemplate struct {
	template.Template

	root block.ReadonlyDevice
	err  error
}

func (t resourceAdmissionTemplate) Rootfs() (block.ReadonlyDevice, error) { return t.root, t.err }

type resourceAdmissionDevice struct {
	block.ReadonlyDevice

	size int64
}

func (d resourceAdmissionDevice) Size(context.Context) (int64, error) { return d.size, nil }

func TestResourceFactoryReconcilesRootfsBeforeSetup(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{600, 801} {
		t.Run(map[int64]string{600: "growth fits", 801: "growth refused"}[size], func(t *testing.T) {
			t.Parallel()
			c := admissionController(t)
			f := &Factory{Admission: c}
			lease, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 500})
			require.NoError(t, err)
			cleanup := NewCleanup()
			got, err := f.prepareResourceAdmission(t.Context(), resourceAdmissionTemplate{root: resourceAdmissionDevice{size: size}}, &Config{Vcpu: 2}, "", "owner", cleanup, lease)
			if size > 800 {
				var refusal *resourceadmission.RefusalError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, resourceadmission.LogicalDisk, refusal.Reason)
			} else {
				require.NoError(t, err)
				require.Same(t, lease, got)
				for _, fs := range c.Report().Filesystems {
					if fs.ID == "root" {
						require.Equal(t, uint64(size), fs.LogicalCommittedBytes)
					}
				}
			}
			lease.ReleaseUnclaimed()
			require.Equal(t, uint64(2), c.Report().CPUCommitted, "factory owns rollback after claiming")
			require.NoError(t, cleanup.Run(t.Context()))
			require.Zero(t, c.Report().CPUCommitted)
		})
	}
}

func TestResourceFactoryRootfsFailureReleasesClaimedLease(t *testing.T) {
	t.Parallel()
	c := admissionController(t)
	f := &Factory{Admission: c}
	lease, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 500})
	require.NoError(t, err)
	cleanup := NewCleanup()
	boom := errors.New("rootfs unavailable")
	_, err = f.prepareResourceAdmission(t.Context(), resourceAdmissionTemplate{err: boom}, &Config{Vcpu: 2}, "", "owner", cleanup, lease)
	require.ErrorIs(t, err, boom)
	require.NoError(t, cleanup.Run(t.Context()))
	require.Zero(t, c.Report().CPUCommitted)
}

func TestResourceFactoryBuildDiskAndDisabledMode(t *testing.T) {
	t.Parallel()
	c := admissionController(t)
	f := &Factory{Admission: c}
	cleanup := NewCleanup()
	lease, err := f.prepareResourceAdmission(t.Context(), resourceAdmissionTemplate{root: resourceAdmissionDevice{size: 500}}, &Config{Vcpu: 1}, "/build/rootfs", "build-vm", cleanup, nil)
	require.NoError(t, err)
	require.NotNil(t, lease)
	for _, fs := range c.Report().Filesystems {
		if fs.ID == "build" {
			require.Equal(t, uint64(500), fs.LogicalCommittedBytes)
		} else {
			require.Zero(t, fs.LogicalCommittedBytes)
		}
	}
	require.NoError(t, cleanup.Run(t.Context()))
	disabled := &Factory{}
	lease, err = disabled.prepareResourceAdmission(t.Context(), nil, nil, "", "", NewCleanup(), nil)
	require.NoError(t, err)
	require.Nil(t, lease)
}

func TestResourceCleanupRetainsFailedEarlyCgroupRemoval(t *testing.T) {
	t.Parallel()
	c := admissionController(t)
	cleanup := boundCleanup(t, c)
	result := errors.New("startup failed before cgroup cleanup registration")
	registered := false
	boom := errors.New("cgroup still owns threads")
	cleanup.cleanupIfNotRegistered(t.Context(), &result, &registered, func(context.Context) error { return boom })
	require.ErrorIs(t, result, boom)
	require.NoError(t, cleanup.Run(t.Context()))
	require.True(t, c.Report().Uncertain)
	require.Equal(t, uint64(2), c.Report().CPUCommitted)
}
