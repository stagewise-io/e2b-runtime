// Package resourceadmission defines the quantities and arithmetic shared by node
// admission and API placement. It does not own runtime resources.
package resourceadmission

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

const (
	Version = 1
	MiB     = uint64(1 << 20)
)

type Reason string

const (
	CPU          Reason = "cpu"
	LogicalDisk  Reason = "logical_disk"
	PhysicalDisk Reason = "physical_disk"
	Measurement  Reason = "invalid_measurement"
	Uncertain    Reason = "uncertain_cleanup"
)

type RefusalError struct{ Reason Reason }

func (e *RefusalError) Error() string { return "node resource admission refused: " + string(e.Reason) }

// Policy is immutable for the lifetime of a host controller.
type Policy struct {
	Enabled       bool
	CPUPercent    uint64
	DiskPercent   uint64
	HeadroomBytes uint64
}

func (p Policy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.CPUPercent == 0 || p.DiskPercent == 0 || p.DiskPercent > 100 || p.HeadroomBytes == 0 {
		return errors.New("enabled resource admission requires positive CPU percent, disk percent 1..100, and positive disk headroom")
	}

	return nil
}

type Resources struct{ CPUs, DiskBytes uint64 }

func FromMiB(cpus, diskMiB int64) (Resources, error) {
	if cpus <= 0 || diskMiB <= 0 {
		return Resources{}, errors.New("CPU and total rootfs MiB must be positive")
	}
	disk, err := Multiply(uint64(diskMiB), MiB)

	return Resources{CPUs: uint64(cpus), DiskBytes: disk}, err
}

func Add(a, b uint64) (uint64, error) {
	if b > math.MaxUint64-a {
		return 0, errors.New("resource quantity overflow")
	}

	return a + b, nil
}

func Multiply(a, b uint64) (uint64, error) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, errors.New("resource quantity overflow")
	}

	return a * b, nil
}

// Percent calculates floor(n*p/100) without overflowing the intermediate product.
func Percent(n, p uint64) (uint64, error) {
	hi, lo := bits.Mul64(n, p)
	if hi >= 100 {
		return 0, errors.New("resource percentage overflow")
	}
	q, _ := bits.Div64(hi, lo, 100)

	return q, nil
}

func CeilMiB(bytes uint64) uint64 {
	return bytes/MiB + boolUint(bytes%MiB != 0)
}

func boolUint(b bool) uint64 {
	if b {
		return 1
	}

	return 0
}

func Fits(limit, held, requested uint64) bool {
	return held <= limit && requested <= limit-held
}

type Filesystem struct {
	ID                                       string
	Roles                                    []string
	TotalBytes, AvailableBytes               uint64
	LogicalLimitBytes, LogicalCommittedBytes uint64
	WriteReservedBytes, HeadroomBytes        uint64
	Valid                                    bool
}

type Report struct {
	Version                   uint32
	Enabled, Valid, Uncertain bool
	CPULimit, CPUCommitted    uint64
	Filesystems               []Filesystem
}

func (r Report) Clone() Report {
	r.Filesystems = slices.Clone(r.Filesystems)
	for i := range r.Filesystems {
		r.Filesystems[i].Roles = slices.Clone(r.Filesystems[i].Roles)
	}
	return r
}

func (r Report) Rootfs() (Filesystem, bool) {
	for _, fs := range r.Filesystems {
		if slices.Contains(fs.Roles, "rootfs") {
			return fs, true
		}
	}

	return Filesystem{}, false
}

// Fit allows conservative local overlays. A node remains the authoritative owner.
func (r Report) Fit(request, pending Resources) error {
	if r.Uncertain {
		return &RefusalError{Uncertain}
	}
	if r.Version != Version || !r.Enabled || !r.Valid {
		return &RefusalError{Measurement}
	}
	cpu, err := Add(pending.CPUs, request.CPUs)
	if err != nil || !Fits(r.CPULimit, r.CPUCommitted, cpu) {
		return &RefusalError{CPU}
	}
	fs, ok := r.Rootfs()
	if !ok || !fs.Valid {
		return &RefusalError{Measurement}
	}
	disk, err := Add(pending.DiskBytes, request.DiskBytes)
	if err != nil || !Fits(fs.LogicalLimitBytes, fs.LogicalCommittedBytes, disk) {
		return &RefusalError{LogicalDisk}
	}
	need, err := Add(fs.WriteReservedBytes, fs.HeadroomBytes)
	if err == nil {
		need, err = Add(need, disk)
	}
	if err != nil || need > fs.AvailableBytes {
		return &RefusalError{PhysicalDisk}
	}
	for _, other := range r.Filesystems {
		if !other.Valid {
			return &RefusalError{Measurement}
		}
		need, err := Add(other.WriteReservedBytes, other.HeadroomBytes)
		if err != nil || need > other.AvailableBytes {
			return &RefusalError{PhysicalDisk}
		}
	}

	return nil
}

func (r Resources) Add(other Resources) (Resources, error) {
	cpu, err := Add(r.CPUs, other.CPUs)
	if err != nil {
		return Resources{}, fmt.Errorf("CPU: %w", err)
	}
	disk, err := Add(r.DiskBytes, other.DiskBytes)

	return Resources{CPUs: cpu, DiskBytes: disk}, err
}
