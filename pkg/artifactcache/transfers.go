package artifactcache

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// A separate bbolt file supplies a namespace-wide OS file lock. Read-only
// opens take shared locks for complete HTTP requests; GC takes an exclusive
// lock. Unlike the metadata DB lock, this covers the entire file transfer.
// Locks are released by the OS when a server crashes. Every peer must use this
// protocol before quota eviction is enabled in a shared store.
func (h *Handler) transferLock(readOnly bool) (*bbolt.DB, error) {
	timeout := 25 * time.Millisecond
	if readOnly {
		timeout = 5 * time.Second
	}
	return bbolt.Open(filepath.Join(h.dir, "transfers.bolt"), 0o600,
		&bbolt.Options{ReadOnly: readOnly, Timeout: timeout, OpenFile: openCoordinationFile})
}

func (h *Handler) prepareTransferLock() error {
	path := filepath.Join(h.dir, "transfers.bolt")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect transfer coordination: %w", err)
	}
	// Initialization is the only operation permitted to create/write this file.
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("initialize transfer coordination: %w", err)
	}
	return db.Close()
}

// bbolt chooses shared/exclusive OS locking from Options.ReadOnly. Its file
// descriptor can stay read-only in either case: this DB is used only as a
// mutex, never for write transactions. This also permits consistent audit
// on a read-only cache mount, and cannot recreate a concurrently removed file.
func openCoordinationFile(name string, _ int, _ os.FileMode) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, fmt.Errorf("transfer coordination must be an existing bounded regular file")
	}
	return os.Open(name)
}
