package placement

import (
	"math"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

// packingScore minimizes mean normalized capacity left after this request.
// CPU and logical disk use the host's admission limits, never best-of-K's
// overcommit or instantaneous CPU usage. Physical disk headroom is a fit check,
// not occupancy: template caches and snapshot writes must not attract creates.
// This is a best-fit heuristic, not an optimizer for future unknown profiles.
func packingScore(node *nodemanager.Node, resources nodemanager.SandboxResources, memory MemoryRequirement) (float64, bool) {
	// Read in-flight memory before metrics, as NodeSatisfiesMemory does, so a
	// completing create is counted in at least one of these two views.
	inFlight := node.PlacementMetrics.InProgress()
	metrics := node.Metrics()
	report, pending, fits := fittingResourceView(node, resources)
	if !fits {
		return 0, false
	}
	fs, _ := report.Rootfs() // fittingResourceView already checked the rootfs.
	request, _ := resourceadmission.FromMiB(resources.CPUs, resources.MiBDisk)
	// Fit proved that these sums cannot overflow and are within their limits.
	cpuLeft := report.CPULimit - report.CPUCommitted - pending.CPUs - request.CPUs
	diskLeft := fs.LogicalLimitBytes - fs.LogicalCommittedBytes - pending.DiskBytes - request.DiskBytes
	score := float64(cpuLeft)/float64(report.CPULimit) + float64(diskLeft)/float64(fs.LogicalLimitBytes)
	dimensions := 2.0

	if resources.HugePages {
		// Never rank an unreported pool as empty. The node still owns final
		// memory admission, including races between independent API replicas.
		if resources.MiBMemory <= 0 || metrics.HugePageSizeBytes == 0 || metrics.HugePagesCapacity == 0 {
			return 0, false
		}
		limit := float64(metrics.HugePagesCapacity)
		if memory.Limit > 0 {
			limit *= memory.Limit
		}
		pageBytes := float64(metrics.HugePageSizeBytes)
		pagesFor := func(miB int64) float64 {
			return math.Ceil(float64(miB) * float64(resourceadmission.MiB) / pageBytes)
		}
		committed := float64(metrics.HugePagesUsed) + float64(metrics.HugePagesReserved) + pagesFor(resources.MiBMemory)
		for _, res := range inFlight {
			if res.HugePages {
				committed += pagesFor(res.MiBMemory)
			}
		}
		if limit <= 0 || (memory.Limit > 0 && committed > limit) {
			return 0, false
		}
		// A disabled placement ceiling leaves admission to the host. Do not
		// reward commitments beyond the reported pool with a negative score.
		score += max(0, limit-committed) / limit
		dimensions++
	}

	return score / dimensions, true
}
