//go:build linux || darwin

package artifactcache

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func availableImportSpace(path string) (uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size")
	}
	blocks, size := stats.Bavail, uint64(stats.Bsize)
	if blocks > math.MaxUint64/size {
		return 0, fmt.Errorf("filesystem free space overflow")
	}
	return blocks * size, nil
}
