package nodemanager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func admissionInfo() *orchestratorinfo.ServiceInfoResponse {
	return &orchestratorinfo.ServiceInfoResponse{MetricDiskAllocatedBytes: 42, ResourceAdmission: (&resourceadmission.Report{Version: resourceadmission.Version, Enabled: true, Valid: true, CPULimit: 20, Filesystems: []resourceadmission.Filesystem{{ID: "dev", Roles: []string{"rootfs"}, Valid: true, TotalBytes: 1000 * resourceadmission.MiB, AvailableBytes: 1000 * resourceadmission.MiB, LogicalLimitBytes: 800 * resourceadmission.MiB, HeadroomBytes: resourceadmission.MiB}}}).Proto()}
}

func TestAdmissionSampleIsolationAndOlderResponse(t *testing.T) {
	t.Parallel()
	n := NewTestNode("n", "ready", 0, 8)
	now := time.Now()
	info := admissionInfo()
	n.UpdateMetricsFromSync(info, now)
	info.ResourceAdmission.Filesystems[0].Roles[0] = "changed"
	metrics := n.Metrics()
	require.Equal(t, uint64(42), metrics.DiskAllocatedBytes)
	require.Equal(t, "rootfs", metrics.ResourceAdmission.Filesystems[0].Roles[0])
	metrics.ResourceAdmission.Filesystems[0].Roles[0] = "mutated"
	report, at, _, err := n.AdmissionView()
	require.NoError(t, err)
	require.Equal(t, now, at)
	require.Equal(t, "rootfs", report.Filesystems[0].Roles[0])
	n.UpdateMetricsFromSync(&orchestratorinfo.ServiceInfoResponse{}, now.Add(-time.Second))
	require.NotNil(t, n.Metrics().ResourceAdmission)
	n.UpdateMetricsFromSync(&orchestratorinfo.ServiceInfoResponse{}, now.Add(time.Second))
	require.Nil(t, n.Metrics().ResourceAdmission)
}

func TestAdmissionSuccessOverlayRetiresOnlyAfterNewRequest(t *testing.T) {
	t.Parallel()
	n := NewTestNode("n", "ready", 0, 8)
	before := time.Now()
	n.UpdateMetricsFromSync(admissionInfo(), before)
	res := SandboxResources{CPUs: 2, MiBDisk: 100}
	attempt := n.PlacementMetrics.BeginPlacingIf("s", res, func() bool { return true })
	attempt.Success()
	_, _, pending, err := n.AdmissionView()
	require.NoError(t, err)
	require.Equal(t, uint64(2), pending.CPUs)
	n.UpdateMetricsFromSync(admissionInfo(), before)
	_, _, pending, err = n.AdmissionView()
	require.NoError(t, err)
	require.Equal(t, uint64(100)*resourceadmission.MiB, pending.DiskBytes)
	n.OptimisticRemove(t.Context(), res)
	_, _, pending, err = n.AdmissionView()
	require.NoError(t, err)
	require.Equal(t, uint64(2), pending.CPUs)
	n.UpdateMetricsFromSync(admissionInfo(), time.Now())
	_, _, pending, err = n.AdmissionView()
	require.NoError(t, err)
	require.Zero(t, pending)
}

func TestAdmissionLegacyOverlayKeepsCPUWithoutPoisoningDisk(t *testing.T) {
	t.Parallel()
	for _, succeeded := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "successful"}[succeeded], func(t *testing.T) {
			t.Parallel()
			n := NewTestNode("n", "ready", 0, 8)
			n.UpdateMetricsFromSync(admissionInfo(), time.Now())
			legacy := n.PlacementMetrics.BeginPlacingIf("legacy", SandboxResources{CPUs: 2}, func() bool { return true })
			if succeeded {
				legacy.Success()
			}
			valid := n.PlacementMetrics.BeginPlacingIf("valid", SandboxResources{CPUs: 3, MiBDisk: 100}, func() bool { return true })
			_, _, pending, err := n.AdmissionView()
			require.NoError(t, err)
			require.Equal(t, resourceadmission.Resources{CPUs: 5, DiskBytes: 100 * resourceadmission.MiB}, pending)
			valid.Fail()
			legacy.Fail()
		})
	}
}

func TestAdmissionAttemptCompletionCannotRemoveReplacement(t *testing.T) {
	t.Parallel()
	n := NewTestNode("n", "ready", 0, 8)
	p := &n.PlacementMetrics
	older := p.BeginPlacingIf("same", SandboxResources{CPUs: 1, MiBDisk: 1}, func() bool { return true })
	newer := p.BeginPlacingIf("same", SandboxResources{CPUs: 2, MiBDisk: 2}, func() bool { return true })
	older.Fail()
	require.Equal(t, int64(2), p.InProgress()["same"].CPUs)
	older.Success()
	newer.Success()
	_, _, pending, err := n.AdmissionView()
	require.NoError(t, err)
	require.Equal(t, uint64(2), pending.CPUs)
	require.Equal(t, uint64(1), p.SuccessCount())
	require.Equal(t, uint64(1), p.FailsCount())
}
