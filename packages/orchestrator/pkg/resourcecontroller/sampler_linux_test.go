//go:build linux

package resourcecontroller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSamplePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := SamplePath(dir)
	require.NoError(t, err)
	require.NotEmpty(t, s.ID)
	var fs unix.Statfs_t
	require.NoError(t, unix.Statfs(dir, &fs))
	require.Equal(t, fs.Blocks*uint64(fs.Bsize), s.TotalBytes)
	// Filesystem writers may run between samples, so test bounds, not equality.
	require.LessOrEqual(t, s.AvailableBytes, s.TotalBytes)
	alias := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(dir, alias))
	aliased, err := SamplePath(alias)
	require.NoError(t, err)
	require.Equal(t, s.ID, aliased.ID)
	_, err = SamplePath(filepath.Join(dir, "missing"))
	require.Error(t, err)
	file := filepath.Join(dir, "regular")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = SamplePath(file)
	require.Error(t, err)
}
