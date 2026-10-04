//go:build !windows

package artifactcache

import (
	"time"

	"go.etcd.io/bbolt"
)

func openTransferLease(path string, readOnly bool, timeout time.Duration) (transferLease, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: readOnly, Timeout: timeout, OpenFile: openCoordinationFile})
	if err != nil {
		return nil, err
	}
	return db, nil
}
