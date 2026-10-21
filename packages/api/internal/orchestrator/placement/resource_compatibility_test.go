package placement

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func admissionReport() resourceadmission.Report {
	return resourceadmission.Report{Version: resourceadmission.Version, Enabled: true, Valid: true, CPULimit: 20, Filesystems: []resourceadmission.Filesystem{{ID: "root", Roles: []string{"rootfs", "build", "template"}, Valid: true, TotalBytes: 1000 * resourceadmission.MiB, AvailableBytes: 1000 * resourceadmission.MiB, LogicalLimitBytes: 800 * resourceadmission.MiB, HeadroomBytes: resourceadmission.MiB}}}
}

func admissionNode(id string, report resourceadmission.Report, requestedAt time.Time) *nodemanager.Node {
	n := nodemanager.NewTestNode(id, api.NodeStatusReady, 0, 8)
	n.UpdateMetricsFromSync(&orchestratorinfo.ServiceInfoResponse{MetricCpuCount: 8, ResourceAdmission: report.Proto()}, requestedAt)

	return n
}

var admissionResources = nodemanager.SandboxResources{CPUs: 2, MiBDisk: 100}

func TestResourceAdmissionFits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*resourceadmission.Report)
		fits   bool
	}{
		{name: "idle", fits: true},
		{name: "exact CPU", fits: true, change: func(r *resourceadmission.Report) { r.CPUCommitted = 18 }},
		{name: "CPU exhausted", change: func(r *resourceadmission.Report) { r.CPUCommitted = 19 }},
		{name: "exact logical disk", fits: true, change: func(r *resourceadmission.Report) {
			r.Filesystems[0].LogicalCommittedBytes = 700 * resourceadmission.MiB
		}},
		{name: "logical disk exhausted", change: func(r *resourceadmission.Report) {
			r.Filesystems[0].LogicalCommittedBytes = 701 * resourceadmission.MiB
		}},
		{name: "exact operation space", fits: true, change: func(r *resourceadmission.Report) { r.Filesystems[0].AvailableBytes = 101 * resourceadmission.MiB }},
		{name: "operation space exhausted", change: func(r *resourceadmission.Report) { r.Filesystems[0].AvailableBytes = 100 * resourceadmission.MiB }},
		{name: "snapshot writes compete", change: func(r *resourceadmission.Report) { r.Filesystems[0].WriteReservedBytes = 900 * resourceadmission.MiB }},
		{name: "uncertain cleanup", change: func(r *resourceadmission.Report) { r.Uncertain = true }},
		{name: "measurement invalid", change: func(r *resourceadmission.Report) { r.Valid = false }},
		{name: "disabled reporter", change: func(r *resourceadmission.Report) { r.Enabled = false }},
		{name: "unsupported version", change: func(r *resourceadmission.Report) { r.Version++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := admissionReport()
			if test.change != nil {
				test.change(&r)
			}
			n := admissionNode("n", r, time.Now())
			require.Equal(t, test.fits, NodeSatisfiesResources(n, admissionResources, true))
			require.True(t, NodeSatisfiesResources(n, admissionResources, false))
		})
	}
}

func TestResourceAdmissionFreshnessAndConversion(t *testing.T) {
	t.Parallel()
	for _, requestedAt := range []time.Time{{}, time.Now().Add(-31 * time.Second), time.Now().Add(time.Minute)} {
		n := admissionNode("stale", admissionReport(), requestedAt)
		require.True(t, NodeSupportsResourceAdmission(n, true))
		require.False(t, NodeSatisfiesResources(n, admissionResources, true))
	}
	n := admissionNode("n", admissionReport(), time.Now())
	for _, res := range []nodemanager.SandboxResources{{CPUs: 0, MiBDisk: 1}, {CPUs: 1, MiBDisk: 0}, {CPUs: 1, MiBDisk: math.MaxInt64}} {
		require.False(t, NodeSatisfiesResources(n, res, true))
	}
}

func TestResourceAdmissionPlacementRoutes(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"preferred", "sampled", "final recheck"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			fullReport := admissionReport()
			fullReport.CPUCommitted = 20
			full := admissionNode("full", fullReport, time.Now())
			full.SetSandboxClient(failingCreateClient(t))
			roomy := admissionNode("roomy", admissionReport(), time.Now())
			algorithm := NewBestOfK(DefaultBestOfKConfig())
			var preferred *nodemanager.Node
			if route == "preferred" {
				preferred = full
			}
			if route == "final recheck" {
				var picks []string
				algorithm = firstNotExcluded([]*nodemanager.Node{full, roomy}, &picks)
			}
			req := testSbxRequest("s")
			req.Sandbox.TotalDiskSizeMb = 100
			result, err := PlaceSandbox(t.Context(), algorithm, []*nodemanager.Node{full, roomy}, preferred, req, Requirements{ResourceAdmission: true}, false, nil)
			require.NoError(t, err)
			require.Equal(t, roomy.ID, result.Node.ID)
			_, _, pending, err := roomy.AdmissionView()
			require.NoError(t, err)
			require.Equal(t, uint64(100)*resourceadmission.MiB, pending.DiskBytes)
		})
	}
}

func TestResourceAdmissionCapabilityVersusCapacity(t *testing.T) {
	t.Parallel()
	algorithm := NewBestOfK(DefaultBestOfKConfig())
	legacy := nodemanager.NewTestNode("legacy", api.NodeStatusReady, 0, 8)
	req := testSbxRequest("s")
	req.Sandbox.TotalDiskSizeMb = 100
	_, err := PlaceSandbox(t.Context(), algorithm, []*nodemanager.Node{legacy}, legacy, req, Requirements{ResourceAdmission: true}, false, nil)
	require.ErrorAs(t, err, &UnsupportedResourceAdmissionError{})
	stale := admissionNode("stale", admissionReport(), time.Now().Add(-time.Minute))
	_, err = PlaceSandbox(t.Context(), algorithm, []*nodemanager.Node{legacy, stale}, nil, req, Requirements{ResourceAdmission: true}, false, nil)
	require.ErrorAs(t, err, &NoNodesAvailableError{})
	result, err := PlaceSandbox(t.Context(), algorithm, []*nodemanager.Node{legacy}, legacy, req, Requirements{}, false, nil)
	require.NoError(t, err)
	require.Equal(t, legacy.ID, result.Node.ID)
}

func TestResourceAdmissionRejectsInvalidBuildResources(t *testing.T) {
	t.Parallel()
	for _, res := range []nodemanager.SandboxResources{{CPUs: 0, MiBDisk: 1}, {CPUs: 1, MiBDisk: 0}, {CPUs: 1, MiBDisk: -1}, {CPUs: 1, MiBDisk: math.MaxInt64}} {
		t.Run(fmt.Sprintf("cpu=%d/disk=%d", res.CPUs, res.MiBDisk), func(t *testing.T) {
			t.Parallel()
			n := admissionNode("n", admissionReport(), time.Now())
			n.SetSandboxClient(failingCreateClient(t))
			req := testSbxRequest("s")
			req.Sandbox.Vcpu, req.Sandbox.TotalDiskSizeMb = res.CPUs, res.MiBDisk
			_, err := PlaceSandbox(t.Context(), NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{n}, n, req, Requirements{ResourceAdmission: true}, false, nil)
			require.ErrorAs(t, err, &InvalidResourceRequestError{})
		})
	}
}

func TestResourceAdmissionDisabledAllowsLegacyMissingDiskSize(t *testing.T) {
	t.Parallel()
	n := admissionNode("n", admissionReport(), time.Now())
	req := testSbxRequest("s")
	req.Sandbox.TotalDiskSizeMb = 0
	result, err := PlaceSandbox(t.Context(), NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{n}, n, req, Requirements{}, false, nil)
	require.NoError(t, err)
	require.Equal(t, n.ID, result.Node.ID)
}

func TestResourceAdmissionFinalCheckSerializesPending(t *testing.T) {
	t.Parallel()
	r := admissionReport()
	r.CPULimit = 2
	n := admissionNode("n", r, time.Now())
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := range 20 {
		wg.Go(func() {
			<-start
			attempt := n.PlacementMetrics.BeginPlacingIf(fmt.Sprintf("s-%d", i), admissionResources, func() bool { return NodeSatisfiesResources(n, admissionResources, true) })
			if attempt != nil {
				admitted.Add(1)
				attempt.Success()
			}
		})
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), admitted.Load())
}
