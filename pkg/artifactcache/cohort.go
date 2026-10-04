package artifactcache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.etcd.io/bbolt"
)

const cohortMarker = "cohort-v1"
const cohortIdentity = "act2-cache-cohort-v1\n"

// enrollCohort holds a shared root lock for the server lifetime. Aggregate
// maintenance takes the exclusive root lock before namespace locks/metadata.
// Only direct child namespaces are allowed. Legacy namespaces are refused.
func enrollCohort(dir, root string) (*bbolt.DB, error) {
	marker := filepath.Join(dir, cohortMarker)
	if root == "" {
		if _, err := os.Lstat(marker); !os.IsNotExist(err) { // #nosec G703 -- explicit caller-selected namespace marker.
			return nil, fmt.Errorf("cohort namespace requires an explicit cohort root")
		}
		return nil, nil
	}
	namespace, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(namespace) != parent || strings.HasPrefix(filepath.Base(namespace), ".") {
		return nil, fmt.Errorf("cache namespace must be a direct child of its cohort root")
	}
	// #nosec G703 -- root is explicitly selected by the local API caller.
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("cohort root must be a directory, not a symlink")
	}
	gate := &Handler{dir: parent}
	if err := gate.prepareTransferLock(); err != nil {
		return nil, err
	}
	lease, err := gate.transferLock(true)
	if err != nil {
		return nil, err
	}
	if err := initializeCohortNamespace(namespace); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func initializeCohortNamespace(dir string) error {
	// #nosec G703 -- caller-selected direct child of the validated cohort root.
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir) // #nosec G703 -- validated direct-child namespace.
	if err != nil || !info.IsDir() {
		return fmt.Errorf("cohort namespace must be a directory")
	}
	marker := filepath.Join(dir, cohortMarker)
	if data, err := readCohortMarker(marker); err == nil { // #nosec G703 -- internal marker in validated namespace.
		if string(data) != cohortIdentity {
			return fmt.Errorf("unsupported cache cohort marker")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := readDirectoryPage(dir, 1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("legacy cache namespace cannot be enrolled without migration")
	}
	// Namespace enrollment is serialized separately from server lifetime leases.
	// The marker uses O_EXCL: concurrent initializers accept only exact identity.
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G703 -- validated namespace marker.
	if os.IsExist(err) {
		data, readErr := readCohortMarker(marker) // #nosec G703 -- internal validated namespace marker.
		if readErr == nil && string(data) == cohortIdentity {
			return nil
		}
	}
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(cohortIdentity)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func readCohortMarker(path string) ([]byte, error) {
	info, err := os.Lstat(path) // #nosec G703 -- explicit internal namespace marker.
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(cohortIdentity)) {
		return nil, fmt.Errorf("invalid cohort marker")
	}
	f, err := os.Open(path) // #nosec G703 -- validated bounded marker.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(len(cohortIdentity)+1)))
}

func readDirectoryPage(path string, limit int) ([]os.DirEntry, error) {
	f, err := os.Open(path) // #nosec G703 -- explicitly selected directory, bounded enumeration.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(limit)
	if err == io.EOF {
		err = nil
	}
	return entries, err
}
