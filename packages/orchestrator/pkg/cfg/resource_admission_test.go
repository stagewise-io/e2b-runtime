//go:build linux

package cfg

import (
	"math"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"
)

func TestResourceAdmissionConfig(t *testing.T) {
	for _, c := range []ResourceAdmissionConfig{{Enabled: true}, {Enabled: true, CPUPercent: 250, DiskPercent: 101, HeadroomMiB: 1}, {Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomMiB: math.MaxUint64}} {
		_, err := c.Policy()
		require.Error(t, err)
	}
	p, err := (ResourceAdmissionConfig{}).Policy()
	require.NoError(t, err)
	require.False(t, p.Enabled)
	t.Setenv("RESOURCE_ADMISSION_ENABLED", "true")
	t.Setenv("RESOURCE_ADMISSION_CPU_PERCENT", "250")
	t.Setenv("RESOURCE_ADMISSION_DISK_PERCENT", "80")
	t.Setenv("RESOURCE_ADMISSION_DISK_HEADROOM_MIB", "100")
	c, err := env.ParseAs[ResourceAdmissionConfig]()
	require.NoError(t, err)
	p, err = c.Policy()
	require.NoError(t, err)
	require.Equal(t, uint64(100<<20), p.HeadroomBytes)
}
