package nodemanager

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
	"github.com/e2b-dev/infra/packages/shared/pkg/smap"
)

type SandboxResources struct {
	CPUs      int64
	MiBMemory int64
	MiBDisk   int64
	// HugePages reports whether the sandbox's RAM comes from the node's
	// hugepage pool rather than ordinary pages.
	HugePages bool
}

type PlacementMetrics struct {
	sandboxesInProgress *smap.Map[SandboxResources]

	// startMu makes StartPlacingIf's check and insert one step per node.
	startMu sync.Mutex

	accountMu   sync.Mutex
	nextAttempt uint64
	current     map[string]uint64
	overlays    map[uint64]placementOverlay

	createSuccess atomic.Uint64
	createFails   atomic.Uint64
}

func (p *PlacementMetrics) Success(sandboxID string) {
	p.createSuccess.Add(1)
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) Skip(sandboxID string) {
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) Fail(sandboxID string) {
	p.createFails.Add(1)
	p.removeSandbox(sandboxID)
}

func (p *PlacementMetrics) SuccessCount() uint64 {
	return p.createSuccess.Load()
}

func (p *PlacementMetrics) FailsCount() uint64 {
	return p.createFails.Load()
}

func (p *PlacementMetrics) InProgress() map[string]SandboxResources {
	return p.sandboxesInProgress.Items()
}

func (p *PlacementMetrics) InProgressCount() uint32 {
	return uint32(p.sandboxesInProgress.Count())
}

// StartPlacingIf records the placement only if fits holds, checking and
// inserting under one lock so concurrent placements cannot all pass a check
// that counts in-flight placements. fits must not call StartPlacingIf.
func (p *PlacementMetrics) StartPlacingIf(sandboxID string, resources SandboxResources, fits func() bool) bool {
	p.startMu.Lock()
	defer p.startMu.Unlock()

	if !fits() {
		return false
	}

	p.begin(sandboxID, resources)

	return true
}

func (p *PlacementMetrics) removeSandbox(sandboxID string) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	delete(p.overlays, p.current[sandboxID])
	delete(p.current, sandboxID)
	p.sandboxesInProgress.Remove(sandboxID)
}

type placementOverlay struct {
	resources   SandboxResources
	succeededAt time.Time
}

// PlacementAttempt qualifies completion by attempt, not just sandbox identity.
// An older RPC cannot remove the pending entry for a newer attempt.
type PlacementAttempt struct {
	metrics   *PlacementMetrics
	sandboxID string
	token     uint64
}

func (p *PlacementMetrics) begin(id string, resources SandboxResources) *PlacementAttempt {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if p.overlays == nil {
		p.overlays = make(map[uint64]placementOverlay)
		p.current = make(map[string]uint64)
	}
	p.nextAttempt++
	p.current[id] = p.nextAttempt
	p.overlays[p.nextAttempt] = placementOverlay{resources: resources}
	p.sandboxesInProgress.Insert(id, resources)

	return &PlacementAttempt{metrics: p, sandboxID: id, token: p.nextAttempt}
}

func (p *PlacementMetrics) BeginPlacingIf(id string, resources SandboxResources, fits func() bool) *PlacementAttempt {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if !fits() {
		return nil
	}

	return p.begin(id, resources)
}

func (a *PlacementAttempt) complete(success, failed bool) {
	p := a.metrics
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	overlay, exists := p.overlays[a.token]
	if !exists || !overlay.succeededAt.IsZero() {
		return
	}
	if success {
		overlay.succeededAt = time.Now()
		p.overlays[a.token] = overlay
		p.createSuccess.Add(1)
	} else {
		delete(p.overlays, a.token)
		if failed {
			p.createFails.Add(1)
		}
	}
	if p.current[a.sandboxID] == a.token {
		delete(p.current, a.sandboxID)
		p.sandboxesInProgress.Remove(a.sandboxID)
	}
}

func (a *PlacementAttempt) Success() { a.complete(true, false) }
func (a *PlacementAttempt) Skip()    { a.complete(false, false) }
func (a *PlacementAttempt) Fail()    { a.complete(false, true) }

// retireSuccesses runs while the node metrics lock holds the replacement report.
func (p *PlacementMetrics) retireSuccesses(requestedAt time.Time) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	for token, overlay := range p.overlays {
		if !overlay.succeededAt.IsZero() && requestedAt.After(overlay.succeededAt) {
			delete(p.overlays, token)
		}
	}
}

func (p *PlacementMetrics) pendingResources() (resourceadmission.Resources, error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	var total resourceadmission.Resources
	for _, overlay := range p.overlays {
		// Legacy flag-off builds may have no total disk size. Preserve their
		// CPU overlay without letting absent disk metadata poison other teams'
		// admission views. The node still enforces actual disk on construction.
		disk := overlay.resources.MiBDisk
		if disk == 0 {
			disk = 1
		}
		res, err := resourceadmission.FromMiB(overlay.resources.CPUs, disk)
		if err != nil {
			return total, err
		}
		if overlay.resources.MiBDisk == 0 {
			res.DiskBytes = 0
		}
		total, err = total.Add(res)
		if err != nil {
			return total, err
		}
	}

	return total, nil
}
