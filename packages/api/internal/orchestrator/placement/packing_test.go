package placement

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

func packingNode(id string, cpu, disk, pages uint64, state api.NodeStatus, options ...nodemanager.TestOptions) *nodemanager.Node {
	n := nodemanager.NewTestNode(id, state, 0, 8, options...)
	r := admissionReport()
	r.CPUCommitted = cpu
	r.Filesystems[0].LogicalCommittedBytes = disk * resourceadmission.MiB
	n.UpdateMetricsFromSync(&orchestratorinfo.ServiceInfoResponse{
		MetricCpuCount: 8, ResourceAdmission: r.Proto(),
		MetricHugepagesTotal: 1000, MetricHugepagesCapacity: 1000,
		MetricHugepagesUsed: pages, MetricHugepageSizeBytes: 2 * resourceadmission.MiB,
	}, time.Now())

	return n
}

func TestPackingUsesStableTiesWithinSample(t *testing.T) {
	t.Parallel()
	config := DefaultBestOfKConfig()
	config.K = 3
	// These knobs must not change node-authoritative packing limits.
	config.R, config.Alpha, config.ScoreHugepages = 0.01, 100, false
	algorithm := NewBestOfK(config)
	nodes := []*nodemanager.Node{
		packingNode("empty-a", 0, 0, 0, api.NodeStatusReady),
		packingNode("populated-z", 12, 500, 0, api.NodeStatusReady),
		packingNode("populated-a", 12, 500, 0, api.NodeStatusReady),
	}
	for range 50 {
		n, err := algorithm.chooseNode(t.Context(), nodes, nil, admissionResources, Requirements{ResourceAdmission: true}, false, nil)
		require.NoError(t, err)
		require.Equal(t, "populated-a", n.ID)
	}
}

func TestPackingHonorsSampleSize(t *testing.T) {
	t.Parallel()
	config := DefaultBestOfKConfig()
	config.K = 1
	algorithm := &BestOfK{config: config}
	nodes := []*nodemanager.Node{
		packingNode("empty", 0, 0, 0, api.NodeStatusReady),
		packingNode("busy", 12, 500, 0, api.NodeStatusReady),
	}
	for range 20 {
		candidates, _ := algorithm.sample(nodes, config, nil, admissionResources, Requirements{ResourceAdmission: true}, false, nil)
		require.Len(t, candidates, config.K)
	}
	seenEmpty := false
	for range 100 {
		n, err := algorithm.chooseNode(t.Context(), nodes, nil, admissionResources, Requirements{ResourceAdmission: true}, false, nil)
		require.NoError(t, err)
		if n.ID == "empty" {
			seenEmpty = true
		}
	}
	// With K=1 either host can win. Scoring all hosts would always choose busy.
	// The probability of never sampling empty is 2^-100.
	require.True(t, seenEmpty, "packing must not expand K to the fleet size")
}

func TestPackingSamplesOnlyFittingHugepagePools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		info *orchestratorinfo.ServiceInfoResponse
	}{
		{name: "unreported pool", info: &orchestratorinfo.ServiceInfoResponse{}},
		{name: "unreported page size", info: &orchestratorinfo.ServiceInfoResponse{MetricHugepagesCapacity: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := DefaultBestOfKConfig()
			config.K = 1
			algorithm := &BestOfK{config: config}
			resources := nodemanager.SandboxResources{CPUs: 2, MiBDisk: 100, MiBMemory: 200, HugePages: true}
			requirements := Requirements{ResourceAdmission: true}
			info := tc.info
			report := admissionReport()
			info.ResourceAdmission = report.Proto()
			bad := nodemanager.NewTestNode("bad", api.NodeStatusReady, 0, 8)
			bad.UpdateMetricsFromSync(info, time.Now())
			good := packingNode("good", 0, 0, 0, api.NodeStatusReady)
			nodes := []*nodemanager.Node{bad, good}
			// The legacy check allows these hosts with its limit disabled.
			require.True(t, NodeSatisfiesMemory(bad, resources, requirements.Memory))
			for range 100 {
				candidates, _ := algorithm.sample(nodes, config, nil, resources, requirements, false, nil)
				require.Len(t, candidates, 1)
				require.Equal(t, good.ID, candidates[0].ID)
				chosen, err := algorithm.chooseNode(t.Context(), nodes, nil, resources, requirements, false, nil)
				require.NoError(t, err)
				require.Equal(t, good.ID, chosen.ID)
			}
			candidates, poolsFull := algorithm.sample([]*nodemanager.Node{bad}, config, nil, resources, requirements, false, nil)
			require.Empty(t, candidates)
			require.True(t, poolsFull)
			_, err := algorithm.chooseNode(t.Context(), []*nodemanager.Node{bad}, nil, resources, requirements, false, nil)
			require.ErrorAs(t, err, new(NoNodesAvailableError))
		})
	}
}

func TestPackingHonorsDisabledHugepageLimit(t *testing.T) {
	t.Parallel()
	resources := nodemanager.SandboxResources{CPUs: 2, MiBDisk: 100, MiBMemory: 200, HugePages: true}
	full := packingNode("full", 0, 0, 1000, api.NodeStatusReady)
	for _, limit := range []float64{0, -1} {
		score, fits := packingScore(full, resources, MemoryRequirement{Limit: limit})
		require.True(t, fits)
		require.InDelta(t, (0.9+0.875)/3, score, 1e-12)
		chosen, err := NewBestOfK(DefaultBestOfKConfig()).chooseNode(t.Context(), []*nodemanager.Node{full}, nil, resources, Requirements{ResourceAdmission: true, Memory: MemoryRequirement{Limit: limit}}, false, nil)
		require.NoError(t, err)
		require.Equal(t, full.ID, chosen.ID)
	}
	for _, limit := range []float64{0.95, 1} {
		_, fits := packingScore(full, resources, MemoryRequirement{Limit: limit})
		require.False(t, fits)
	}
}

func TestPackingScoreUsesRequestedResourcesAndLimits(t *testing.T) {
	t.Parallel()
	n := packingNode("n", 10, 300, 0, api.NodeStatusReady)
	score, fits := packingScore(n, admissionResources, MemoryRequirement{})
	require.True(t, fits)
	require.InDelta(t, (0.4+0.5)/2, score, 1e-12)

	// Successful creates remain in the CPU/disk view until a fresh sync.
	attempt := n.PlacementMetrics.BeginPlacingIf("pending", admissionResources, func() bool { return true })
	require.NotNil(t, attempt)
	attempt.Success()
	score, fits = packingScore(n, admissionResources, MemoryRequirement{})
	require.True(t, fits)
	require.InDelta(t, (0.3+0.375)/2, score, 1e-12)

	r := admissionReport()
	r.CPUCommitted = 12
	r.Filesystems[0].LogicalCommittedBytes = 400 * resourceadmission.MiB
	n.UpdateMetricsFromSync(&orchestratorinfo.ServiceInfoResponse{ResourceAdmission: r.Proto()}, time.Now())
	scoreAfterSync, fits := packingScore(n, admissionResources, MemoryRequirement{})
	require.True(t, fits)
	require.InDelta(t, score, scoreAfterSync, 1e-12)
}

func TestPackingDoesNotPreferPhysicalDiskPressure(t *testing.T) {
	t.Parallel()
	r := admissionReport()
	r.Filesystems[0].AvailableBytes = r.Filesystems[0].HeadroomBytes + 200*resourceadmission.MiB
	pressured := admissionNode("pressured", r, time.Now())
	empty := admissionNode("empty", admissionReport(), time.Now())
	score, fits := packingScore(pressured, admissionResources, MemoryRequirement{})
	require.True(t, fits)
	emptyScore, fits := packingScore(empty, admissionResources, MemoryRequirement{})
	require.True(t, fits)
	require.InDelta(t, emptyScore, score, 1e-12)
}

func TestPackingHugepages(t *testing.T) {
	t.Parallel()
	res := nodemanager.SandboxResources{CPUs: 2, MiBDisk: 100, MiBMemory: 200, HugePages: true}
	busy := packingNode("busy", 0, 0, 800, api.NodeStatusReady)
	empty := packingNode("empty", 0, 0, 0, api.NodeStatusReady)
	algorithm := NewBestOfK(DefaultBestOfKConfig())
	req := Requirements{ResourceAdmission: true, Memory: MemoryRequirement{Limit: 0.95}}
	n, err := algorithm.chooseNode(t.Context(), []*nodemanager.Node{empty, busy}, nil, res, req, false, nil)
	require.NoError(t, err)
	require.Equal(t, busy.ID, n.ID)

	// Each pending request rounds up separately to a whole hugepage.
	busy.PlacementMetrics.StartPlacingIf("pending", nodemanager.SandboxResources{CPUs: 1, MiBDisk: 1, MiBMemory: 101, HugePages: true}, func() bool { return true })
	n, err = algorithm.chooseNode(t.Context(), []*nodemanager.Node{empty, busy}, nil, res, req, false, nil)
	require.NoError(t, err)
	require.Equal(t, empty.ID, n.ID)

	// Missing pools are unavailable, not artificially empty packing targets.
	missing := admissionNode("missing", admissionReport(), time.Now())
	_, fits := packingScore(missing, res, MemoryRequirement{})
	require.False(t, fits)
	// Ordinary-page requests must neither use nor require a hugepage pool.
	res.HugePages = false
	_, fits = packingScore(missing, res, MemoryRequirement{})
	require.True(t, fits)
}

func TestPackingSkipsInvalidOrFullReports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*resourceadmission.Report)
	}{
		{name: "CPU full", change: func(r *resourceadmission.Report) { r.CPUCommitted = r.CPULimit }},
		{name: "logical disk full", change: func(r *resourceadmission.Report) {
			r.Filesystems[0].LogicalCommittedBytes = r.Filesystems[0].LogicalLimitBytes
		}},
		{name: "physical disk full", change: func(r *resourceadmission.Report) { r.Filesystems[0].AvailableBytes = r.Filesystems[0].HeadroomBytes }},
		{name: "write reservation", change: func(r *resourceadmission.Report) {
			r.Filesystems[0].WriteReservedBytes = r.Filesystems[0].AvailableBytes
		}},
		{name: "invalid", change: func(r *resourceadmission.Report) { r.Valid = false }},
		{name: "uncertain", change: func(r *resourceadmission.Report) { r.Uncertain = true }},
		{name: "unsupported", change: func(r *resourceadmission.Report) { r.Version++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := admissionReport()
			tc.change(&r)
			n := admissionNode("bad", r, time.Now())
			_, fits := packingScore(n, admissionResources, MemoryRequirement{})
			require.False(t, fits)
			good := admissionNode("good", admissionReport(), time.Now())
			chosen, err := NewBestOfK(DefaultBestOfKConfig()).chooseNode(t.Context(), []*nodemanager.Node{n, good}, nil, admissionResources, Requirements{ResourceAdmission: true}, false, nil)
			require.NoError(t, err)
			require.Equal(t, good.ID, chosen.ID)
		})
	}
	for _, at := range []time.Time{{}, time.Now().Add(-time.Minute), time.Now().Add(time.Minute)} {
		n := admissionNode("stale", admissionReport(), at)
		_, fits := packingScore(n, admissionResources, MemoryRequirement{})
		require.False(t, fits)
	}
	n := admissionNode("n", admissionReport(), time.Now())
	_, fits := packingScore(n, nodemanager.SandboxResources{CPUs: 2, MiBDisk: math.MaxInt64}, MemoryRequirement{})
	require.False(t, fits)
}

func TestPackingPreservesEligibility(t *testing.T) {
	t.Parallel()
	for _, state := range []api.NodeStatus{api.NodeStatusDraining, api.NodeStatusUnhealthy} {
		busy := packingNode("busy", 12, 500, 0, state)
		empty := packingNode("empty", 0, 0, 0, api.NodeStatusReady)
		n, err := NewBestOfK(DefaultBestOfKConfig()).chooseNode(t.Context(), []*nodemanager.Node{busy, empty}, nil, admissionResources, Requirements{ResourceAdmission: true}, false, nil)
		require.NoError(t, err)
		require.Equal(t, empty.ID, n.ID)
	}
	busy := packingNode("busy", 12, 500, 0, api.NodeStatusReady, nodemanager.WithLabels([]string{"wrong"}), nodemanager.WithCPUInfo("x86_64", "Intel", "wrong"), nodemanager.WithOrchestratorVersion("0.8.0"))
	empty := packingNode("empty", 0, 0, 0, api.NodeStatusReady, nodemanager.WithLabels([]string{"required"}), nodemanager.WithCPUInfo("x86_64", "Intel", "right"), nodemanager.WithOrchestratorVersion("0.12.0"))
	for _, tc := range []struct {
		requirements Requirements
		excluded     map[string]struct{}
		labels       bool
	}{
		{requirements: Requirements{ResourceAdmission: true}, excluded: map[string]struct{}{busy.ID: {}}},
		{requirements: Requirements{ResourceAdmission: true}, labels: true},
		{requirements: Requirements{ResourceAdmission: true, CPU: CPURequirement{PinnedModel: "right"}}},
		{requirements: Requirements{ResourceAdmission: true, Features: Require(testFeatureHigh)}},
	} {
		n, err := NewBestOfK(DefaultBestOfKConfig()).chooseNode(t.Context(), []*nodemanager.Node{busy, empty}, tc.excluded, admissionResources, tc.requirements, tc.labels, []string{"required"})
		require.NoError(t, err)
		require.Equal(t, empty.ID, n.ID)
	}
}

func TestPackingProfilesConsolidateAndOverflow(t *testing.T) {
	t.Parallel()
	nodes := []*nodemanager.Node{packingNode("a", 0, 0, 0, api.NodeStatusReady), packingNode("b", 0, 0, 0, api.NodeStatusReady)}
	algorithm := NewBestOfK(DefaultBestOfKConfig())
	for i, profile := range []struct{ cpu, disk int64 }{{2, 100}, {4, 200}, {8, 400}, {2, 100}, {2, 100}} {
		req := testSbxRequest(fmt.Sprintf("s-%d", i))
		req.Sandbox.Vcpu, req.Sandbox.TotalDiskSizeMb = profile.cpu, profile.disk
		result, err := PlaceSandbox(t.Context(), algorithm, nodes, nil, req, Requirements{ResourceAdmission: true}, false, nil)
		require.NoError(t, err)
		want := "a"
		if i == 4 {
			want = "b"
		}
		require.Equal(t, want, result.Node.ID)
	}
}

func TestPackingCapacityRefusalTriesOtherHost(t *testing.T) {
	t.Parallel()
	busy := packingNode("busy", 12, 500, 0, api.NodeStatusReady)
	empty := packingNode("empty", 0, 0, 0, api.NodeStatusReady)
	calls := 0
	busy.SetSandboxClient(&nodemanager.MockSandboxClientCustom{CreateFunc: func() error {
		calls++

		return status.Error(codes.ResourceExhausted, "stale report")
	}})
	req := testSbxRequest("s")
	req.Sandbox.TotalDiskSizeMb = 100
	result, err := PlaceSandbox(t.Context(), NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{busy, empty}, nil, req, Requirements{ResourceAdmission: true}, false, nil)
	require.NoError(t, err)
	require.Equal(t, empty.ID, result.Node.ID)
	require.Equal(t, 1, calls)
	require.Zero(t, busy.PlacementMetrics.FailsCount())
}

func TestPackingCapacityRefusalRemainsRetryable(t *testing.T) {
	t.Parallel()
	n := packingNode("n", 0, 0, 0, api.NodeStatusReady)
	calls := 0
	n.SetSandboxClient(&nodemanager.MockSandboxClientCustom{CreateFunc: func() error {
		calls++
		if calls == 1 {
			return status.Error(codes.ResourceExhausted, "temporary")
		}

		return nil
	}})
	req := testSbxRequest("s")
	req.Sandbox.TotalDiskSizeMb = 100
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := PlaceSandbox(ctx, NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{n}, nil, req, Requirements{ResourceAdmission: true}, false, nil)
	require.NoError(t, err)
	require.Equal(t, n.ID, result.Node.ID)
	require.Equal(t, 2, calls)
}

func TestPackingResumeAffinityAndDrain(t *testing.T) {
	t.Parallel()
	for _, state := range []api.NodeStatus{api.NodeStatusReady, api.NodeStatusDraining} {
		origin := packingNode("origin", 0, 0, 0, state)
		busy := packingNode("busy", 12, 500, 0, api.NodeStatusReady)
		req := testSbxRequest("s")
		req.Sandbox.TotalDiskSizeMb = 100
		result, err := PlaceSandbox(t.Context(), NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{origin, busy}, origin, req, Requirements{ResourceAdmission: true}, false, nil)
		require.NoError(t, err)
		want := origin.ID
		if state == api.NodeStatusDraining {
			want = busy.ID
		}
		require.Equal(t, want, result.Node.ID)
	}
}
