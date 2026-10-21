package resourceadmission

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckedQuantities(t *testing.T) {
	t.Parallel()
	n, err := Percent(8, 250)
	require.NoError(t, err)
	require.Equal(t, uint64(20), n)
	n, err = Percent(math.MaxUint64, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(math.MaxUint64), n)
	n, err = Percent(50, math.MaxUint64)
	require.NoError(t, err)
	require.Equal(t, uint64(math.MaxUint64/2), n)
	_, err = Percent(math.MaxUint64, 101)
	require.Error(t, err)
	_, err = Add(math.MaxUint64, 1)
	require.Error(t, err)
	_, err = Multiply(math.MaxUint64, MiB)
	require.Error(t, err)
	for _, v := range []int64{-1, 0, math.MaxInt64} {
		_, err := FromMiB(1, v)
		require.Error(t, err)
	}
	require.Equal(t, uint64(2), CeilMiB(MiB+1))
	require.True(t, Fits(20, 16, 4))
	require.False(t, Fits(20, 21, 0))
	require.False(t, Fits(20, 16, 5))
}

func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	require.NoError(t, (Policy{}).Validate())
	require.Error(t, (Policy{Enabled: true}).Validate())
	require.Error(t, (Policy{Enabled: true, CPUPercent: 250, DiskPercent: 101, HeadroomBytes: 1}).Validate())
	require.NoError(t, (Policy{Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomBytes: 1}).Validate())
}

func TestFit(t *testing.T) {
	t.Parallel()
	r := Report{Version: Version, Enabled: true, Valid: true, CPULimit: 20, CPUCommitted: 16, Filesystems: []Filesystem{{ID: "disk", Roles: []string{"rootfs"}, Valid: true, LogicalLimitBytes: 100, LogicalCommittedBytes: 60, AvailableBytes: 50, HeadroomBytes: 10}}}
	require.NoError(t, r.Fit(Resources{CPUs: 4, DiskBytes: 40}, Resources{}))
	require.ErrorContains(t, r.Fit(Resources{CPUs: 5}, Resources{}), string(CPU))
	require.ErrorContains(t, r.Fit(Resources{CPUs: 4, DiskBytes: 41}, Resources{}), string(LogicalDisk))
	r.Filesystems[0].WriteReservedBytes = 1
	require.ErrorContains(t, r.Fit(Resources{CPUs: 4, DiskBytes: 40}, Resources{}), string(PhysicalDisk))
	r.Filesystems[0].WriteReservedBytes = 0
	r.Filesystems = append(r.Filesystems, Filesystem{ID: "build", Roles: []string{"build"}, Valid: true, AvailableBytes: 9, HeadroomBytes: 10})
	require.ErrorContains(t, r.Fit(Resources{}, Resources{}), string(PhysicalDisk))
	r.Filesystems[1].AvailableBytes = 10
	require.NoError(t, r.Fit(Resources{CPUs: 4, DiskBytes: 40}, Resources{}))
	r.Valid = false
	require.ErrorContains(t, r.Fit(Resources{}, Resources{}), string(Measurement))
	r.Uncertain = true
	require.ErrorContains(t, r.Fit(Resources{}, Resources{}), string(Uncertain))
}
