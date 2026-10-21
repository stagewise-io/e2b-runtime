package resourcecontroller

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

type reader struct {
	mu      sync.Mutex
	samples map[string]Sample
	err     error
}

func (r *reader) sample(path string) (Sample, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.samples[path], r.err
}

func (r *reader) set(path string, s Sample) { r.mu.Lock(); defer r.mu.Unlock(); r.samples[path] = s }
func (r *reader) fail(err error)            { r.mu.Lock(); defer r.mu.Unlock(); r.err = err }

func fixture(t *testing.T, separate bool) (*Controller, *reader) {
	t.Helper()
	r := &reader{samples: map[string]Sample{Rootfs: {ID: "a", TotalBytes: 1000, AvailableBytes: 1000}, Build: {ID: "a", TotalBytes: 1000, AvailableBytes: 1000}, Template: {ID: "a", TotalBytes: 1000, AvailableBytes: 1000}}}
	if separate {
		r.samples[Build] = Sample{ID: "b", TotalBytes: 2000, AvailableBytes: 2000}
	}
	c, err := New(Options{Policy: resourceadmission.Policy{Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomBytes: 100}, CPUCount: 8, Paths: map[string]string{Rootfs: Rootfs, Build: Build, Template: Template}, Sample: r.sample})
	require.NoError(t, err)

	return c, r
}

func reason(t *testing.T, err error, want resourceadmission.Reason) {
	t.Helper()
	var refusal *resourceadmission.RefusalError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, want, refusal.Reason)
}

func TestEnabledControllerRequiresMeasurements(t *testing.T) {
	t.Parallel()
	policy := resourceadmission.Policy{Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomBytes: 100}
	for _, o := range []Options{
		{Policy: resourceadmission.Policy{Enabled: true}},
		{Policy: policy},
		{Policy: policy, CPUCount: 8, Sample: func(string) (Sample, error) { return Sample{}, nil }},
		{Policy: policy, CPUCount: 8, Paths: map[string]string{Rootfs: Rootfs}, Sample: func(string) (Sample, error) { return Sample{ID: "a", TotalBytes: 1000, AvailableBytes: 1000}, nil }},
	} {
		_, err := New(o)
		require.Error(t, err)
	}
	c, _ := fixture(t, false)
	_, err := c.Acquire(resourceadmission.Resources{CPUs: 1})
	require.Error(t, err)
	_, err = c.AcquireWrites("unknown", 1)
	require.Error(t, err)
	_, err = c.AcquireWrites(Build, math.MaxUint64)
	reason(t, err, resourceadmission.PhysicalDisk)
}

func TestConcurrentAdmission(t *testing.T) {
	t.Parallel()
	for range 100 {
		c, _ := fixture(t, false)
		start := make(chan struct{})
		results := make(chan *Lease, 2)
		errs := make(chan error, 2)
		// These callers represent independent clients, both with stale empty
		// reports. Either request fits alone; their sum cannot fit.
		for range 2 {
			go func() {
				<-start
				l, err := c.Acquire(resourceadmission.Resources{CPUs: 12, DiskBytes: 500})
				results <- l
				errs <- err
			}()
		}
		close(start)
		var winner *Lease
		successes := 0
		for range 2 {
			if l := <-results; l != nil {
				winner = l
				successes++
			}
		}
		require.Equal(t, 1, successes)
		failures := 0
		for range 2 {
			if err := <-errs; err != nil {
				failures++
				reason(t, err, resourceadmission.CPU)
			}
		}
		require.Equal(t, 1, failures)
		r := c.Report()
		require.Equal(t, uint64(12), r.CPUCommitted)
		winner.Release(nil)
		l, err := c.Acquire(resourceadmission.Resources{CPUs: 20, DiskBytes: 800})
		require.NoError(t, err)
		l.Release(nil)
	}
}

func TestLifecycleOwnershipAndCleanup(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	old, err := c.Acquire(resourceadmission.Resources{CPUs: 10, DiskBytes: 400})
	require.NoError(t, err)
	require.NoError(t, old.Claim("old"))
	require.NoError(t, old.Claim("old"))
	require.Error(t, old.Claim("new"))
	old.Started()
	old.Started()
	root, _ := c.Report().Rootfs()
	require.Zero(t, root.WriteReservedBytes)
	require.Equal(t, uint64(400), root.LogicalCommittedBytes)
	// Same customer sandbox identity can own a replacement lifecycle while
	// old cleanup is held. The controller never keys resources by sandbox ID.
	replacement, err := c.Acquire(resourceadmission.Resources{CPUs: 10, DiskBytes: 400})
	require.NoError(t, err)
	require.NoError(t, replacement.Claim("replacement"))
	_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
	reason(t, err, resourceadmission.CPU)
	old.Release(nil)
	old.Release(errors.New("stale callback"))
	old.Started()
	require.False(t, c.Report().Uncertain)
	require.Equal(t, uint64(10), c.Report().CPUCommitted)
	replacement.Release(nil)
	replacement.Release(nil)
	require.Zero(t, c.Report().CPUCommitted)
	require.Error(t, old.Claim("old"))
}

func TestFailedCleanupRetainsCapacity(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		c, _ := fixture(t, false)
		l, err := c.Acquire(resourceadmission.Resources{CPUs: 5, DiskBytes: 200})
		require.NoError(t, err)
		if started {
			l.Started()
		}
		l.Release(errors.New("cleanup failed"))
		l.Release(nil)
		l.Started()
		r := c.Report()
		require.True(t, r.Uncertain)
		require.Equal(t, uint64(5), r.CPUCommitted)
		root, _ := r.Rootfs()
		require.Equal(t, uint64(200), root.LogicalCommittedBytes)
		if !started {
			require.Equal(t, uint64(200), root.WriteReservedBytes)
		}
		_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
		reason(t, err, resourceadmission.Uncertain)
		_, err = c.AcquireWrites(Build, 1)
		reason(t, err, resourceadmission.Uncertain)
	}
}

func TestGrowBeforeVMSetup(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	l, err := c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 100})
	require.NoError(t, err)
	require.NoError(t, l.Grow(800))
	require.NoError(t, l.Grow(10))
	root, _ := c.Report().Rootfs()
	require.Equal(t, uint64(800), root.LogicalCommittedBytes)
	require.Equal(t, uint64(800), root.WriteReservedBytes)
	reason(t, l.Grow(801), resourceadmission.LogicalDisk)
	reason(t, l.Grow(math.MaxUint64), resourceadmission.LogicalDisk)
	root, _ = c.Report().Rootfs()
	require.Equal(t, uint64(800), root.LogicalCommittedBytes)
	l.Started()
	require.Error(t, l.Grow(900))
	l.Release(nil)
	root, _ = c.Report().Rootfs()
	require.Zero(t, root.LogicalCommittedBytes)
	require.Zero(t, root.WriteReservedBytes)
}

func TestSharedFilesystemWriteAdmission(t *testing.T) {
	t.Parallel()
	c, r := fixture(t, false)
	require.Len(t, c.Report().Filesystems, 1)
	l, err := c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 500})
	require.NoError(t, err)
	w, err := c.AcquireWrites(Build, 400)
	require.NoError(t, err)
	_, err = c.AcquireWrites(Template, 1)
	reason(t, err, resourceadmission.PhysicalDisk)
	_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
	reason(t, err, resourceadmission.PhysicalDisk)
	// Written bytes are now reflected in Bavail. Dropping that portion of
	// the estimate must not create additional capacity.
	for _, path := range []string{Rootfs, Build, Template} {
		r.set(path, Sample{ID: "a", TotalBytes: 1000, AvailableBytes: 800})
	}
	require.NoError(t, w.Reduce(200))
	require.Error(t, w.Reduce(300))
	_, err = c.AcquireWrites(Build, 1)
	reason(t, err, resourceadmission.PhysicalDisk)
	w.Release(nil)
	w.Release(nil)
	require.NoError(t, w.Reduce(0))
	l.Release(nil)
	root, _ := c.Report().Rootfs()
	require.Zero(t, root.WriteReservedBytes)
}

func TestSeparateFilesystemsAndHeadroom(t *testing.T) {
	t.Parallel()
	c, r := fixture(t, true)
	require.Len(t, c.Report().Filesystems, 2)
	l, err := c.Acquire(resourceadmission.Resources{CPUs: 20, DiskBytes: 800})
	require.NoError(t, err)
	w, err := c.AcquireWrites(Build, 1900)
	require.NoError(t, err)
	_, err = c.AcquireWrites(Build, 1)
	reason(t, err, resourceadmission.PhysicalDisk)
	w.Release(nil)
	l.Release(nil)
	r.set(Build, Sample{ID: "b", TotalBytes: 2000, AvailableBytes: 99})
	_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
	reason(t, err, resourceadmission.PhysicalDisk)
}

func TestBadMeasurementsNeverLookIdle(t *testing.T) {
	t.Parallel()
	c, r := fixture(t, false)
	l, err := c.Acquire(resourceadmission.Resources{CPUs: 2, DiskBytes: 100})
	require.NoError(t, err)
	for _, sample := range []Sample{{}, {ID: "a", TotalBytes: 10, AvailableBytes: 11}, {ID: "changed", TotalBytes: 1000, AvailableBytes: 1000}} {
		r.set(Rootfs, sample)
		report := c.Report()
		require.False(t, report.Valid)
		require.Equal(t, uint64(2), report.CPUCommitted)
		_, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 1})
		reason(t, err, resourceadmission.Measurement)
	}
	r.set(Rootfs, Sample{ID: "a", TotalBytes: 1000, AvailableBytes: 1000})
	r.fail(errors.New("statfs failed"))
	require.False(t, c.Report().Valid)
	r.fail(nil)
	require.True(t, c.Report().Valid)
	l.Release(nil)
}

func TestReportIsImmutableAndDisabledDoesNotSample(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	r := c.Report()
	r.Filesystems[0].Roles[0] = "mutated"
	r.Filesystems[0].LogicalCommittedBytes = 99
	root, ok := c.Report().Rootfs()
	require.True(t, ok)
	require.Zero(t, root.LogicalCommittedBytes)
	disabled, err := New(Options{Sample: func(string) (Sample, error) { panic("disabled controller sampled") }})
	require.NoError(t, err)
	l, err := disabled.Acquire(resourceadmission.Resources{})
	require.NoError(t, err)
	require.NoError(t, l.Grow(math.MaxUint64))
	l.Started()
	l.Release(errors.New("ignored in disabled mode"))
	w, err := disabled.AcquireWrites(Build, math.MaxUint64)
	require.NoError(t, err)
	w.Release(nil)
	r = disabled.Report()
	require.False(t, r.Enabled)
	require.False(t, r.Uncertain)
	require.Zero(t, r.CPUCommitted)
}

func TestConcurrentCreateSnapshotSpace(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	start := make(chan struct{})
	results := make(chan error, 2)
	var vm *Lease
	var write *WriteLease
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		var err error
		vm, err = c.Acquire(resourceadmission.Resources{CPUs: 1, DiskBytes: 600})
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		var err error
		write, err = c.AcquireWrites(Build, 600)
		results <- err
	}()
	close(start)
	wg.Wait()
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else {
			reason(t, err, resourceadmission.PhysicalDisk)
		}
	}
	require.Equal(t, 1, success)
	if vm != nil {
		vm.Release(nil)
	}
	if write != nil {
		write.Release(nil)
	}
	root, _ := c.Report().Rootfs()
	require.Zero(t, root.WriteReservedBytes)
}

func TestWriteCleanupFailureCannotBeUndone(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, false)
	w, err := c.AcquireWrites(Build, 300)
	require.NoError(t, err)
	w.Release(errors.New("upload state unknown"))
	w.Release(nil)
	require.NoError(t, w.Reduce(0))
	r := c.Report()
	require.True(t, r.Uncertain)
	root, _ := r.Rootfs()
	require.Equal(t, uint64(300), root.WriteReservedBytes)
}
