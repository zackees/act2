//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func toolMountID(path string) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, fmt.Errorf("kernel mount identity unavailable")
	}
	return stat.Mnt_id, nil
}

func verifyToolRetirementMounts(ctx context.Context, root, generation string, mountID func(string) (uint64, error)) error {
	expected, err := mountID(root)
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Dir(generation), generation} {
		observed, err := mountID(path)
		if err != nil {
			return err
		}
		if observed != expected {
			return fmt.Errorf("tool generation retirement crosses a mount boundary")
		}
	}
	entries := 0
	return walkToolTreeWithDepth(ctx, generation, 0, 72, func(path string, _ fs.DirEntry) error {
		entries++
		if entries > 1000000 {
			return fmt.Errorf("retirement mount inventory exceeds bound")
		}
		observed, err := mountID(path)
		if err != nil {
			return err
		}
		if observed != expected {
			return fmt.Errorf("tool generation retirement crosses a mount boundary")
		}
		return nil
	})
}
