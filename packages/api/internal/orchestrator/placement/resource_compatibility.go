package placement

import (
	"time"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

const admissionMaxSampleAge = 30 * time.Second

type UnsupportedResourceAdmissionError struct{}

func (UnsupportedResourceAdmissionError) Error() string {
	return "cluster does not support enforced resource admission"
}

func NodeSupportsResourceAdmission(node *nodemanager.Node, enabled bool) bool {
	if !enabled {
		return true
	}
	report := node.Metrics().ResourceAdmission

	return report != nil && report.Version == resourceadmission.Version && report.Enabled
}

// NodeSatisfiesResources uses the node's policy, never the best-of-K scoring
// ratio. It must also run inside the per-node check-and-record critical section.
func NodeSatisfiesResources(node *nodemanager.Node, resources nodemanager.SandboxResources, enabled bool) bool {
	if !enabled {
		return true
	}
	report, requestedAt, pending, err := node.AdmissionView()
	if err != nil || report == nil || requestedAt.IsZero() {
		return false
	}
	age := time.Since(requestedAt)
	if age < 0 || age > admissionMaxSampleAge {
		return false
	}
	request, err := resourceadmission.FromMiB(resources.CPUs, resources.MiBDisk)

	return err == nil && report.Fit(request, pending) == nil
}

func anyNodeSupportsResourceAdmission(nodes []*nodemanager.Node, enabled bool) bool {
	for _, node := range nodes {
		if NodeSupportsResourceAdmission(node, enabled) {
			return true
		}
	}

	return false
}
