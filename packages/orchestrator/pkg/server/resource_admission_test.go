//go:build linux

package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/resourcecontroller"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func snapshotAdmissionController(t *testing.T, separate bool, cpus uint64) *resourcecontroller.Controller {
	t.Helper()
	c, err := resourcecontroller.New(resourcecontroller.Options{
		Policy:   resourceadmission.Policy{Enabled: true, CPUPercent: 100, DiskPercent: 80, HeadroomBytes: uint64(testGiB)},
		CPUCount: cpus,
		Paths:    map[string]string{resourcecontroller.Rootfs: "rootfs", resourcecontroller.Build: "build", resourcecontroller.Template: "template", "snapshot-storage": "storage"},
		Sample: func(path string) (resourcecontroller.Sample, error) {
			id := "shared"
			if separate {
				id = path
			}

			return resourcecontroller.Sample{ID: id, TotalBytes: uint64(16 * testGiB), AvailableBytes: uint64(4 * testGiB)}, nil
		},
	})
	require.NoError(t, err)

	return c
}

func reservedSnapshotBytes(c *resourcecontroller.Controller) uint64 {
	var total uint64
	for _, fs := range c.Report().Filesystems {
		total += fs.WriteReservedBytes
	}

	return total
}

func TestResourceSnapshotWritesShareCreateAdmission(t *testing.T) {
	t.Parallel()
	s := diskAdmissionTestServer(t, -1, 0)
	c := snapshotAdmissionController(t, false, 8)
	s.sandboxFactory.Admission = c
	sbx := diskAdmissionSandbox(t, "shared-ledger", 40, 2048)
	r, err := s.admitPauseDisk(t.Context(), sbx, false)
	require.NoError(t, err, "enabled admission applies even with legacy admission off")
	defer r.Release()
	require.Equal(t, uint64(2*testGiB+pauseSnapfileAllowance), reservedSnapshotBytes(c))
	_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: uint64(testGiB)})
	var refusal *resourceadmission.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, resourceadmission.PhysicalDisk, refusal.Reason)
	r.Written()
	lease, err := c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: uint64(testGiB)})
	require.NoError(t, err)
	lease.ReleaseUnclaimed()
}

func TestResourceSnapshotWritesUseDestinationDevices(t *testing.T) {
	t.Parallel()
	for _, separate := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "separate"}[separate], func(t *testing.T) {
			t.Parallel()
			s := diskAdmissionTestServer(t, -1, 0)
			c := snapshotAdmissionController(t, separate, 8)
			s.sandboxFactory.Admission = c
			s.templateStorage.Provider = storage.LocalStorageProvider
			sbx := diskAdmissionSandbox(t, "destinations", 41, 1024)
			r, err := s.admitCheckpointDisk(t.Context(), sbx, false)
			require.NoError(t, err)
			defer r.Release()
			need := uint64(testGiB + pauseSnapfileAllowance)
			require.Equal(t, 2*need, reservedSnapshotBytes(c))
			if separate {
				for _, fs := range c.Report().Filesystems {
					switch fs.ID {
					case "rootfs":
						require.Zero(t, fs.WriteReservedBytes)
					case "build":
						require.Equal(t, uint64(testGiB), fs.WriteReservedBytes)
					case "template":
						require.Equal(t, uint64(pauseSnapfileAllowance), fs.WriteReservedBytes)
					case "storage":
						require.Equal(t, need, fs.WriteReservedBytes)
					}
				}
			}
			r.Written()
			r.Written()
			require.Equal(t, need, reservedSnapshotBytes(c), "local second copy stays charged until upload completion")
			r.Release()
			r.Release()
			require.Zero(t, reservedSnapshotBytes(c))
		})
	}
}

func TestResourceSnapshotWritesWaitForSealAndUpload(t *testing.T) {
	t.Parallel()
	for _, uploadFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "seal-first", true: "upload-first"}[uploadFirst], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := diskAdmissionTestServer(t, -1, 0)
				c := snapshotAdmissionController(t, true, 8)
				s.sandboxFactory.Admission = c
				s.templateStorage.Provider = storage.LocalStorageProvider
				r, err := s.admitCheckpointDisk(t.Context(), diskAdmissionSandbox(t, "seal", 42, 1024), false)
				require.NoError(t, err)
				seal := make(chan struct{})
				res := &snapshotResult{
					rootfsDiff: &build.NoDiff{}, memoryExportDeferred: true,
					waitMemorySealed: func(context.Context) error {
						<-seal

						return nil
					},
					completeUpload: func(context.Context, error) {}, abandonUpload: func(context.Context, error) {},
				}
				s.bindSnapshotWrites(t.Context(), res, r)
				synctest.Wait()
				if uploadFirst {
					res.abandonUpload(t.Context(), errors.New("discarded"))
					synctest.Wait()
					require.Equal(t, uint64(2*(testGiB+pauseSnapfileAllowance)), reservedSnapshotBytes(c))
					close(seal)
				} else {
					close(seal)
					synctest.Wait()
					require.Equal(t, uint64(testGiB+pauseSnapfileAllowance), reservedSnapshotBytes(c))
					res.completeUpload(t.Context(), nil)
				}
				synctest.Wait()
				require.Zero(t, reservedSnapshotBytes(c))
				require.Zero(t, s.info.OutstandingWork())
			})
		})
	}
}

func TestResourceSnapshotFailureWaitsForCleanupProof(t *testing.T) {
	t.Parallel()
	for _, cleanupFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "failed"}[cleanupFails], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := diskAdmissionTestServer(t, -1, 0)
				c := snapshotAdmissionController(t, false, 8)
				s.sandboxFactory.Admission = c
				r, err := s.admitPauseDisk(t.Context(), diskAdmissionSandbox(t, "cleanup-proof", 50, 1024), false)
				require.NoError(t, err)
				done := make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				s.finishFailedSnapshotWrites(ctx, r, &sandbox.SnapshotError{
					Err: errors.New("capture failed"),
					WaitCleanup: func(ctx context.Context) error {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						<-done
						if cleanupFails {
							return errors.New("cleanup unconfirmed")
						}

						return nil
					},
				})
				synctest.Wait()
				require.Positive(t, reservedSnapshotBytes(c))
				require.Positive(t, s.info.OutstandingWork())
				require.False(t, c.Report().Uncertain)
				close(done)
				synctest.Wait()
				require.Zero(t, s.info.OutstandingWork())
				require.Equal(t, cleanupFails, c.Report().Uncertain)
				if cleanupFails {
					require.Positive(t, reservedSnapshotBytes(c))
				} else {
					require.Zero(t, reservedSnapshotBytes(c))
				}
			})
		})
	}
}

func TestResourceSnapshotPreWriteFailureReleases(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := diskAdmissionTestServer(t, -1, 0)
		c := snapshotAdmissionController(t, false, 8)
		s.sandboxFactory.Admission = c
		r, err := s.admitCheckpointDisk(t.Context(), diskAdmissionSandbox(t, "pre-write", 51, 1024), false)
		require.NoError(t, err)
		s.finishFailedSnapshotWrites(t.Context(), r, &sandbox.SnapshotError{
			Err:         errors.New("metadata missing before capture"),
			WaitCleanup: func(context.Context) error { return nil },
		})
		synctest.Wait()
		require.Zero(t, reservedSnapshotBytes(c))
		require.False(t, c.Report().Uncertain)
		require.Zero(t, s.info.OutstandingWork())
	})
}

func TestResourceSnapshotFailureRetainsUncertainWrites(t *testing.T) {
	t.Parallel()
	s := diskAdmissionTestServer(t, -1, 0)
	c := snapshotAdmissionController(t, false, 8)
	s.sandboxFactory.Admission = c
	r, err := s.admitCheckpointDisk(t.Context(), diskAdmissionSandbox(t, "failed-snapshot", 43, 1024), false)
	require.NoError(t, err)
	s.finishFailedSnapshotWrites(t.Context(), r, errors.New("snapshot workers may still be active"))
	require.True(t, c.Report().Uncertain)
	require.Positive(t, reservedSnapshotBytes(c))
}

type resourceAdmissionDevice struct{ *admissionRODevice }

func (resourceAdmissionDevice) Size(context.Context) (int64, error) { return testGiB, nil }

type resourceAdmissionTemplate struct{ admissionTestTemplate }

func (resourceAdmissionTemplate) Metadata() (metadata.Template, error) {
	return metadata.Template{VcpuCount: 2}, nil
}

func (resourceAdmissionTemplate) Rootfs() (block.ReadonlyDevice, error) {
	return resourceAdmissionDevice{}, nil
}

func TestResourceCheckpointReplacementRefusesBeforeDestruction(t *testing.T) {
	t.Parallel()
	s, sbx, steps, _ := slowResumeServer(t)
	c := snapshotAdmissionController(t, false, 2)
	s.sandboxFactory.Admission = c
	sbx.Template = resourceAdmissionTemplate{}
	sbx.Config = sandbox.NewConfig(sandbox.Config{Vcpu: 2})
	original, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: uint64(testGiB)})
	require.NoError(t, err)
	defer original.ReleaseUnclaimed()
	_, err = s.checkpointResumeFreshWith(t.Context(), steps, sbx, &orchestrator.SandboxCheckpointRequest{SandboxId: sbx.Runtime.SandboxID}, func() {})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	_, live := s.sandboxFactory.Sandboxes.Get(sbx.Runtime.SandboxID)
	require.True(t, live)
	require.Zero(t, steps.asyncStops.Load())
	require.Zero(t, steps.stopAndCloses.Load())
	select {
	case <-steps.snapshotStarted:
		t.Fatal("snapshot started before replacement admission")
	default:
	}
	require.Equal(t, uint64(2), c.Report().CPUCommitted)
	require.Equal(t, uint64(testGiB), reservedSnapshotBytes(c))
}

type admissionBlockedDiff struct {
	build.NoDiff

	done <-chan struct{}
}

func (d admissionBlockedDiff) CachePath(context.Context) (string, error) {
	<-d.done

	return "", nil
}

func TestResourceSnapshotSealFailureStillJoinsRootfs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := diskAdmissionTestServer(t, -1, 0)
		c := snapshotAdmissionController(t, false, 8)
		s.sandboxFactory.Admission = c
		r, err := s.admitCheckpointDisk(t.Context(), diskAdmissionSandbox(t, "seal-failed", 45, 1024), false)
		require.NoError(t, err)
		rootfsDone := make(chan struct{})
		res := &snapshotResult{
			rootfsDiff: &admissionBlockedDiff{done: rootfsDone}, memoryExportDeferred: true,
			waitMemorySealed: func(context.Context) error { return errors.New("seal failed") },
			completeUpload:   func(context.Context, error) {}, abandonUpload: func(context.Context, error) {},
		}
		s.bindSnapshotWrites(t.Context(), res, r)
		res.abandonUpload(t.Context(), errors.New("discarded"))
		synctest.Wait()
		require.Equal(t, uint64(testGiB+pauseSnapfileAllowance), reservedSnapshotBytes(c))
		require.Positive(t, s.info.OutstandingWork(), "rootfs writer must still be joined")
		close(rootfsDone)
		synctest.Wait()
		require.True(t, c.Report().Uncertain)
		require.Equal(t, uint64(testGiB+pauseSnapfileAllowance), reservedSnapshotBytes(c))
		require.Zero(t, s.info.OutstandingWork())
	})
}

func TestResourceHarvestRefusalSkipsOptimization(t *testing.T) {
	t.Parallel()
	c := snapshotAdmissionController(t, false, 2)
	original, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: uint64(testGiB)})
	require.NoError(t, err)
	defer original.ReleaseUnclaimed()
	resumer := factoryResumer{factory: &sandbox.Factory{Admission: c}}
	sbx := testHarvestSandbox()
	instance, err := resumer.ResumeForHarvest(t.Context(), resourceAdmissionTemplate{}, &sandbox.Config{Vcpu: 2}, sbx.Runtime, sbx.GetStartedAt(), sbx.GetEndAt(), "")
	var refusal *resourceadmission.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, resourceadmission.CPU, refusal.Reason)
	require.Nil(t, instance)
	probe := newHarvestProbe()
	probe.resumer.err = err
	_, outcome, harvestErr := probe.run(t.Context(), true)
	require.ErrorAs(t, harvestErr, &refusal)
	require.Equal(t, harvestResumeFailed, outcome)
	require.True(t, probe.released)
	require.Equal(t, probe.tmpls.pins, probe.tmpls.releases)
	require.False(t, probe.upload.called)
	require.False(t, probe.tmpls.updated)
	require.Equal(t, uint64(2), c.Report().CPUCommitted)
	require.False(t, c.Report().Uncertain)
}

func TestResourceCheckpointBusyReleasesPreAdmission(t *testing.T) {
	t.Parallel()
	s, sbx, steps, _ := slowResumeServer(t)
	s.featureFlags = diskAdmissionFlags(t, -1)
	c := snapshotAdmissionController(t, false, 4)
	s.sandboxFactory.Admission = c
	sbx.Template = resourceAdmissionTemplate{}
	sbx.Config = sandbox.NewConfig(sandbox.Config{Vcpu: 2})
	original, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: uint64(testGiB)})
	require.NoError(t, err)
	defer original.ReleaseUnclaimed()
	require.NoError(t, original.Claim("original"))
	original.Started()
	defer original.Release(nil)
	require.True(t, sbx.BeginCheckpoint())
	defer sbx.EndCheckpoint()
	_, err = s.checkpointResumeFreshWith(t.Context(), steps, sbx, &orchestrator.SandboxCheckpointRequest{SandboxId: sbx.Runtime.SandboxID}, func() {})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, uint64(2), c.Report().CPUCommitted)
	require.Zero(t, reservedSnapshotBytes(c))
}

func TestResourceInPlaceCheckpointRefusesDiskWithoutStopping(t *testing.T) {
	t.Parallel()
	s := diskAdmissionTestServer(t, -1, 0)
	c := snapshotAdmissionController(t, false, 8)
	s.sandboxFactory.Admission = c
	sbx := diskAdmissionSandbox(t, "in-place-capacity", 44, 4096)
	require.NoError(t, s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx))
	_, _, err := s.checkpointInPlace(t.Context(), sbx, &orchestrator.SandboxCheckpointRequest{SandboxId: sbx.Runtime.SandboxID})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	_, live := s.sandboxFactory.Sandboxes.Get(sbx.Runtime.SandboxID)
	require.True(t, live)
	require.True(t, sbx.BeginCheckpoint(), "refusal must release the checkpoint marker")
	sbx.EndCheckpoint()
	require.Zero(t, c.Report().CPUCommitted)
	require.Zero(t, reservedSnapshotBytes(c))
}

type capacityRefusingCheckpointSteps struct {
	*slowResumeSteps

	reason resourceadmission.Reason
}

func (f capacityRefusingCheckpointSteps) resume(context.Context, sbxtemplate.Template, *sandbox.Sandbox) (*sandbox.Sandbox, error) {
	return nil, &resourceadmission.RefusalError{Reason: f.reason}
}

func TestResourceCheckpointLateRefusalIsFatalWithDetails(t *testing.T) {
	t.Parallel()
	for _, reason := range []resourceadmission.Reason{resourceadmission.CPU, resourceadmission.LogicalDisk, resourceadmission.PhysicalDisk} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s, sbx, steps, _ := slowResumeServer(t)
				_, err := s.checkpointResumeFreshWith(t.Context(), capacityRefusingCheckpointSteps{steps, reason}, sbx,
					&orchestrator.SandboxCheckpointRequest{SandboxId: sbx.Runtime.SandboxID}, func() {})
				require.Equal(t, codes.Internal, status.Code(err), "the original sandbox cannot be restored to Running")
				details := status.Convert(err).Details()
				require.Len(t, details, 1)
				detail, ok := details[0].(*errdetails.ErrorInfo)
				require.True(t, ok)
				require.Equal(t, "e2b.orchestrator", detail.GetDomain())
				require.Equal(t, "RESOURCE_ADMISSION_"+strings.ToUpper(string(reason)), detail.GetReason())
				require.Empty(t, detail.GetMetadata())
				synctest.Wait()
				_, live := s.sandboxFactory.Sandboxes.Get(sbx.Runtime.SandboxID)
				require.False(t, live)
				require.Positive(t, steps.stopAndCloses.Load())
				require.Equal(t, int32(1), steps.asyncStops.Load())
				require.Zero(t, s.info.OutstandingWork())
			})
		})
	}
}

func TestResourceAdmissionStatusDetails(t *testing.T) {
	t.Parallel()
	for reason, expected := range map[resourceadmission.Reason]string{
		resourceadmission.CPU:          "RESOURCE_ADMISSION_CPU",
		resourceadmission.LogicalDisk:  "RESOURCE_ADMISSION_LOGICAL_DISK",
		resourceadmission.PhysicalDisk: "RESOURCE_ADMISSION_PHYSICAL_DISK",
		resourceadmission.Measurement:  "RESOURCE_ADMISSION_INVALID_MEASUREMENT",
		resourceadmission.Uncertain:    "RESOURCE_ADMISSION_UNCERTAIN_CLEANUP",
	} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			st := status.Convert(resourceAdmissionStatus(&resourceadmission.RefusalError{Reason: reason}))
			require.Equal(t, codes.ResourceExhausted, st.Code())
			require.Equal(t, "node resource capacity unavailable, please retry", st.Message())
			details := status.FromProto(st.Proto()).Details()
			require.Len(t, details, 1)
			detail, ok := details[0].(*errdetails.ErrorInfo)
			require.True(t, ok)
			require.Equal(t, "e2b.orchestrator", detail.GetDomain())
			require.Equal(t, expected, detail.GetReason())
			require.Empty(t, detail.GetMetadata(), "host quantities must not reach customers")
		})
	}
	ordinary := errors.New("non-admission failure")
	require.ErrorIs(t, resourceAdmissionStatus(ordinary), ordinary)
	duplicate := status.Error(codes.AlreadyExists, "duplicate")
	require.ErrorIs(t, resourceAdmissionStatus(duplicate), duplicate)
}
