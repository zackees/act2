//go:build linux

package artifactcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	boltErrors "go.etcd.io/bbolt/errors"
)

// Ordinary updates serialize complete read/merge/publish/select operations.
// Their existing operation context bounds admission as well as publication;
// a busy catalog is not evidence of a damaged store. Other reader and exclusive
// probes retain their short timeout and missing coordination never gets rebuilt.
func prepareToolGenerationUpdateCatalog(ctx context.Context, root string) (transferLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Join(root, toolStoreMarker))
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("tool generation admission requires an established store")
	}
	return prepareToolSnapshotStoreWithLease(root, func(path string) (transferLease, error) {
		return openToolGenerationUpdateCatalog(ctx, path)
	})
}

func openToolGenerationUpdateCatalog(ctx context.Context, path string) (transferLease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timeout := 100 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, context.DeadlineExceeded
			}
			if remaining < timeout {
				timeout = remaining
			}
		}
		lease, err := openTransferLease(path, false, timeout)
		if contextErr := ctx.Err(); contextErr != nil {
			if lease != nil {
				_ = lease.Close()
			}
			return nil, contextErr
		}
		if !errors.Is(err, boltErrors.ErrTimeout) {
			return lease, err
		}
		// Only legitimate contention is admitted again. Each open uses the
		// original existing descriptor protocol and the same operation deadline.
	}
}
