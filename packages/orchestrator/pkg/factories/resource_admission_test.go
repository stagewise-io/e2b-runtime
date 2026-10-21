//go:build linux

package factories

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
)

func TestResourceAdmissionRequiresReclaim(t *testing.T) {
	t.Parallel()
	c := cfg.Config{}
	_, err := newResourceController(c, false, false)
	require.NoError(t, err)
	c.ResourceAdmission = cfg.ResourceAdmissionConfig{Enabled: true, CPUPercent: 250, DiskPercent: 80, HeadroomMiB: 1}
	for _, state := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		_, err = newResourceController(c, state[0], state[1])
		require.ErrorContains(t, err, "successful startup reclaim")
	}
	dir := t.TempDir()
	c.DefaultCacheDir = dir
	c.TemplatesDir = dir
	c.StorageConfig.SandboxCacheDir = dir
	c.StorageConfig.TemplateCacheDir = dir
	controller, err := newResourceController(c, true, true)
	require.NoError(t, err)
	require.True(t, controller.Report().Enabled)
	require.Len(t, controller.Report().Filesystems, 1)
}
