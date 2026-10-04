package artifactcache

import (
	"errors"
	"os"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	boltErrors "go.etcd.io/bbolt/errors"
	"golang.org/x/sys/windows"
)

// Shared leases retain bbolt's existing read-only validation and locking.
// Exclusive leases validate that same immutable coordination file read-only,
// then lock its descriptor directly: writable-mode bbolt mmap truncates the
// file on Windows even when its descriptor was deliberately opened read-only.
func openTransferLease(path string, readOnly bool, timeout time.Duration) (transferLease, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: timeout, OpenFile: openCoordinationFile})
	if err != nil {
		return nil, err
	}
	if readOnly {
		return db, nil
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	file, err := openCoordinationFile(path, 0, 0)
	if err != nil {
		return nil, err
	}
	lease := &windowsTransferLease{file: file, overlap: windows.Overlapped{Offset: ^uint32(0), OffsetHigh: ^uint32(0)}}
	// The byte range exactly matches bbolt's Windows flock implementation,
	// preserving exclusion against shared bbolt lifetime/HTTP leases.
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lease.overlap)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, boltErrors.ErrTimeout
		}
		return nil, err
	}
	return lease, nil
}

type windowsTransferLease struct {
	file    *os.File
	overlap windows.Overlapped
	once    sync.Once
	err     error
}

func (lease *windowsTransferLease) Close() error {
	lease.once.Do(func() {
		lease.err = errors.Join(windows.UnlockFileEx(windows.Handle(lease.file.Fd()), 0, 1, 0, &lease.overlap), lease.file.Close())
	})
	return lease.err
}
