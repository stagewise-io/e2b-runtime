// Package resourcecontroller owns host-wide resource leases independently of
// sandbox routing and operation reservations. A single controller must be shared
// by every runtime role in the orchestrator process.
package resourcecontroller

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

const (
	Rootfs   = "rootfs"
	Build    = "build"
	Template = "template"
)

// Sample describes the filesystem containing a configured storage path.
// ID is the filesystem device identity, not a mountpoint or a path.
type Sample struct {
	ID                         string
	TotalBytes, AvailableBytes uint64
}

type Options struct {
	Policy   resourceadmission.Policy
	CPUCount uint64
	Paths    map[string]string
	// Sample does only local filesystem measurement. It must not reenter the
	// controller. Measurements and ledger updates share the admission mutex.
	Sample func(string) (Sample, error)
}

type filesystem struct {
	id              string
	roles           []string
	logical, writes uint64
}

type Controller struct {
	mu                     sync.Mutex
	policy                 resourceadmission.Policy
	cpuLimit, cpuCommitted uint64
	paths                  map[string]string
	roleIDs                map[string]string
	filesystems            map[string]*filesystem
	sample                 func(string) (Sample, error)
	uncertain              bool
	rejections             map[resourceadmission.Reason]uint64
}

func New(o Options) (*Controller, error) {
	if err := o.Policy.Validate(); err != nil {
		return nil, err
	}
	c := &Controller{rejections: make(map[resourceadmission.Reason]uint64), policy: o.Policy, paths: make(map[string]string), roleIDs: make(map[string]string), filesystems: make(map[string]*filesystem), sample: o.Sample}
	if !o.Policy.Enabled {
		return c, nil
	}
	if o.Sample == nil || o.CPUCount == 0 {
		return nil, errors.New("resource admission requires CPU and filesystem measurements")
	}
	var err error
	c.cpuLimit, err = resourceadmission.Percent(o.CPUCount, o.Policy.CPUPercent)
	if err != nil || c.cpuLimit == 0 {
		return nil, errors.New("invalid resource admission CPU limit")
	}
	roles := []string{Rootfs, Build, Template}
	for role := range o.Paths {
		if !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	slices.Sort(roles)
	for _, role := range roles {
		path := o.Paths[role]
		if path == "" {
			return nil, fmt.Errorf("missing resource admission path for %s", role)
		}
		s, err := o.Sample(path)
		if err != nil {
			return nil, fmt.Errorf("measure %s filesystem: %w", role, err)
		}
		if !validSample(s) {
			return nil, fmt.Errorf("invalid %s filesystem measurement", role)
		}
		c.paths[role] = path
		c.roleIDs[role] = s.ID
		fs := c.filesystems[s.ID]
		if fs == nil {
			fs = &filesystem{id: s.ID}
			c.filesystems[s.ID] = fs
		}
		fs.roles = append(fs.roles, role)
	}
	if r := c.reportLocked(); !r.Valid {
		return nil, errors.New("invalid resource admission filesystem policy")
	}

	return c, nil
}

func validSample(s Sample) bool {
	return s.ID != "" && s.TotalBytes > 0 && s.AvailableBytes <= s.TotalBytes
}

// Report returns a deep snapshot. A failed or changed-device measurement keeps
// existing commitments visible but makes new admission fail closed.
func (c *Controller) Report() resourceadmission.Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.reportLocked()
}

func (c *Controller) reportLocked() resourceadmission.Report {
	r := resourceadmission.Report{Version: resourceadmission.Version, Enabled: c.policy.Enabled, Valid: true, Uncertain: c.uncertain, CPULimit: c.cpuLimit, CPUCommitted: c.cpuCommitted}
	if !c.policy.Enabled {
		return r
	}
	ids := make([]string, 0, len(c.filesystems))
	for id := range c.filesystems {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fs := c.filesystems[id]
		f := resourceadmission.Filesystem{ID: id, Roles: slices.Clone(fs.roles), Valid: true, LogicalCommittedBytes: fs.logical, WriteReservedBytes: fs.writes, HeadroomBytes: c.policy.HeadroomBytes}
		// Read every configured path, including bind aliases. A path redirected
		// to another device cannot silently keep using the old ledger.
		for i, role := range fs.roles {
			s, err := c.sample(c.paths[role])
			if err != nil || !validSample(s) || s.ID != id {
				f.Valid = false

				continue
			}
			if i == 0 {
				f.TotalBytes = s.TotalBytes
				f.AvailableBytes = s.AvailableBytes
			} else {
				if s.TotalBytes != f.TotalBytes {
					f.Valid = false
				}
				f.AvailableBytes = min(f.AvailableBytes, s.AvailableBytes)
			}
		}
		var err error
		f.LogicalLimitBytes, err = resourceadmission.Percent(f.TotalBytes, c.policy.DiskPercent)
		if err != nil || f.LogicalLimitBytes == 0 {
			f.Valid = false
		}
		r.Valid = r.Valid && f.Valid
		r.Filesystems = append(r.Filesystems, f)
	}

	return r
}

func (c *Controller) checkLocked(r resourceadmission.Report) error {
	if c.uncertain {
		return &resourceadmission.RefusalError{Reason: resourceadmission.Uncertain}
	}
	if !r.Valid {
		return &resourceadmission.RefusalError{Reason: resourceadmission.Measurement}
	}
	// Even an operation on one filesystem must leave every relevant device
	// above headroom. Never sum capacity across separate devices.
	for _, fs := range r.Filesystems {
		need, err := resourceadmission.Add(fs.WriteReservedBytes, fs.HeadroomBytes)
		if err != nil || need > fs.AvailableBytes {
			return &resourceadmission.RefusalError{Reason: resourceadmission.PhysicalDisk}
		}
	}

	return nil
}

// Lease belongs to a physical VM lifecycle, not a sandbox ID. All mutable fields
// are guarded by its controller. The caller must release only after rollback or
// physical cleanup has completed, including asynchronous cleanup work.
type Lease struct {
	controller         *Controller
	resources          resourceadmission.Resources
	owner              string
	filesystem         string
	starting, released bool
}

func (c *Controller) Acquire(resources resourceadmission.Resources) (_ *Lease, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.recordRefusalLocked(err) }()

	return c.acquireLocked(resources, c.roleIDs[Rootfs])
}

// AcquireAt charges a direct build rootfs on its own filesystem.
func (c *Controller) AcquireAt(resources resourceadmission.Resources, path string) (_ *Lease, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.recordRefusalLocked(err) }()
	if !c.policy.Enabled {
		return c.acquireLocked(resources, "")
	}
	s, err := c.sample(path)
	if err != nil || !validSample(s) || c.filesystems[s.ID] == nil {
		return nil, &resourceadmission.RefusalError{Reason: resourceadmission.Measurement}
	}

	return c.acquireLocked(resources, s.ID)
}

func (c *Controller) acquireLocked(resources resourceadmission.Resources, id string) (*Lease, error) {
	l := &Lease{controller: c, resources: resources, starting: true, filesystem: id}
	if !c.policy.Enabled {
		return l, nil
	}
	if resources.CPUs == 0 || resources.DiskBytes == 0 {
		return nil, errors.New("admission requires positive CPU and rootfs size")
	}
	r := c.reportLocked()
	if err := c.checkLocked(r); err != nil {
		return nil, err
	}
	if err := fitFilesystem(r, id, resources); err != nil {
		return nil, err
	}
	c.cpuCommitted += resources.CPUs
	fs := c.filesystems[id]
	fs.logical += resources.DiskBytes
	fs.writes += resources.DiskBytes

	return l, nil
}

// fitFilesystem reuses the shared predicate for a non-runtime rootfs device.
func fitFilesystem(r resourceadmission.Report, id string, resources resourceadmission.Resources) error {
	for i := range r.Filesystems {
		r.Filesystems[i].Roles = nil
		if r.Filesystems[i].ID == id {
			r.Filesystems[i].Roles = []string{Rootfs}
		}
	}

	return r.Fit(resources, resourceadmission.Resources{})
}

func (c *Controller) Enabled() bool { return c != nil && c.policy.Enabled }

// ReleaseUnclaimed is for a caller whose factory never accepted ownership.
func (l *Lease) ReleaseUnclaimed() {
	if l == nil {
		return
	}
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.owner == "" {
		l.releaseLocked(nil)
	}
}

// ValidateStart rejects a lease borrowed from another host, resource request or
// filesystem. It is only valid while construction still owns the start estimate.
func (l *Lease) ValidateStart(c *Controller, cpus int64, directPath string) error {
	l.controller.mu.Lock()
	defer l.controller.mu.Unlock()
	if c != l.controller || cpus <= 0 || uint64(cpus) > l.resources.CPUs || l.released || !l.starting {
		return errors.New("invalid incoming resource lease")
	}
	id := c.roleIDs[Rootfs]
	if directPath != "" {
		s, err := c.sample(directPath)
		if err != nil || !validSample(s) {
			return errors.New("cannot measure incoming lease filesystem")
		}
		id = s.ID
	}
	if l.filesystem != id {
		return errors.New("incoming lease belongs to another filesystem")
	}

	return nil
}

// Claim transfers an incoming start lease to exactly one cleanup owner.
// Repeating the transfer to the same lifecycle is harmless; reuse is an error.
func (l *Lease) Claim(owner string) error {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner == "" || l.released || (l.owner != "" && l.owner != owner) {
		return errors.New("resource lease already owned or released")
	}
	l.owner = owner

	return nil
}

// Grow reconciles request metadata against the authoritative rootfs size before
// VM setup. Reductions never release a commitment, and a failed growth is atomic.
func (l *Lease) Grow(diskBytes uint64) (err error) {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.recordRefusalLocked(err) }()
	if l.released || !l.starting {
		return errors.New("cannot resize completed resource lease")
	}
	if diskBytes <= l.resources.DiskBytes {
		return nil
	}
	if !c.policy.Enabled {
		l.resources.DiskBytes = diskBytes

		return nil
	}
	delta := diskBytes - l.resources.DiskBytes
	r := c.reportLocked()
	if err := c.checkLocked(r); err != nil {
		return err
	}
	if err := fitFilesystem(r, l.filesystem, resourceadmission.Resources{DiskBytes: delta}); err != nil {
		return err
	}
	fs := c.filesystems[l.filesystem]
	fs.logical += delta
	fs.writes += delta
	l.resources.DiskBytes = diskBytes

	return nil
}

// Started drops only the start-space estimate. CPU and logical disk stay charged
// until Release confirms physical cleanup. Repeated notifications are harmless.
func (l *Lease) Started() {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.released || !l.starting {
		return
	}
	if c.policy.Enabled {
		c.filesystems[l.filesystem].writes -= l.resources.DiskBytes
	}
	l.starting = false
}

// Release retains the entire affected lease on cleanup failure. No later stale
// callback can undo that failure or make the node appear safe again.
func (l *Lease) Release(cleanupErr error) {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	l.releaseLocked(cleanupErr)
}

func (l *Lease) releaseLocked(cleanupErr error) {
	c := l.controller
	if l.released {
		return
	}
	l.released = true
	if !c.policy.Enabled {
		return
	}
	if cleanupErr != nil {
		c.uncertain = true

		return
	}
	c.cpuCommitted -= l.resources.CPUs
	fs := c.filesystems[l.filesystem]
	fs.logical -= l.resources.DiskBytes
	if l.starting {
		fs.writes -= l.resources.DiskBytes
	}
}

// WriteLease accounts for operation estimates, such as pause and checkpoint
// writes. Materialized bytes can be dropped after the filesystem accounts for
// them; unwritten second-copy estimates must remain until persistence completes.
type WriteLease struct {
	controller *Controller
	filesystem string
	bytes      uint64
	released   bool
}

func (c *Controller) AcquireWrites(role string, bytes uint64) (_ *WriteLease, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.recordRefusalLocked(err) }()
	l := &WriteLease{controller: c, filesystem: c.roleIDs[role], bytes: bytes}
	if !c.policy.Enabled {
		return l, nil
	}
	if l.filesystem == "" {
		return nil, errors.New("unknown resource admission filesystem role")
	}
	r := c.reportLocked()
	if err := c.checkLocked(r); err != nil {
		return nil, err
	}
	for _, fs := range r.Filesystems {
		if fs.ID != l.filesystem {
			continue
		}
		need, err := resourceadmission.Add(fs.WriteReservedBytes, fs.HeadroomBytes)
		if err == nil {
			need, err = resourceadmission.Add(need, bytes)
		}
		if err != nil || need > fs.AvailableBytes {
			return nil, &resourceadmission.RefusalError{Reason: resourceadmission.PhysicalDisk}
		}
	}
	c.filesystems[l.filesystem].writes += bytes

	return l, nil
}

// RegisterPath adds a configured storage role before the server accepts work.
func (c *Controller) RegisterPath(role, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.policy.Enabled {
		return nil
	}
	if _, exists := c.paths[role]; exists {
		return errors.New("resource admission role already registered")
	}
	s, err := c.sample(path)
	if err != nil || !validSample(s) {
		return errors.New("cannot measure resource admission storage path")
	}
	c.paths[role], c.roleIDs[role] = path, s.ID
	fs := c.filesystems[s.ID]
	if fs == nil {
		fs = &filesystem{id: s.ID}
		c.filesystems[s.ID] = fs
	}
	fs.roles = append(fs.roles, role)

	return nil
}

// Reduce records a smaller outstanding estimate, after Written or completion.
// Increasing an estimate requires another atomic admission, not a blind update.
func (l *WriteLease) Reduce(bytes uint64) error {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.released {
		return nil
	}
	if bytes > l.bytes {
		return errors.New("cannot increase a write reservation without admission")
	}
	if c.policy.Enabled {
		c.filesystems[l.filesystem].writes -= l.bytes - bytes
	}
	l.bytes = bytes

	return nil
}

func (l *WriteLease) Release(cleanupErr error) {
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if !c.policy.Enabled {
		return
	}
	if cleanupErr != nil {
		c.uncertain = true

		return
	}
	c.filesystems[l.filesystem].writes -= l.bytes
}
