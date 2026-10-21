//go:build linux

package resourcecontroller

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/resourceadmission"
)

// SamplePath measures the actual configured storage directory, including bind
// mounts. Bavail excludes blocks reserved for the host, even when run as root.
func SamplePath(path string) (Sample, error) {
	// Measure one open directory so a path/mount change cannot mix device
	// identity from one filesystem with capacity from another.
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Sample{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Sample{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return Sample{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || fs.Bsize <= 0 || fs.Bavail > fs.Blocks {
		return Sample{}, errors.New("invalid storage directory filesystem")
	}
	total, err := resourceadmission.Multiply(fs.Blocks, uint64(fs.Bsize))
	if err != nil {
		return Sample{}, err
	}
	available, err := resourceadmission.Multiply(fs.Bavail, uint64(fs.Bsize))
	if err != nil {
		return Sample{}, err
	}
	s := Sample{ID: fmt.Sprintf("dev:%x", st.Dev), TotalBytes: total, AvailableBytes: available}
	if !validSample(s) {
		return Sample{}, errors.New("invalid storage filesystem measurement")
	}

	return s, nil
}
