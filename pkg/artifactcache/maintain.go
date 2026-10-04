package artifactcache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
	boltErrors "go.etcd.io/bbolt/errors"
)

// MaintainStore applies retention without starting an HTTP server. The lock
// spans before/after accounting and removal. Unknown/untracked data refuses
// collection; missing stores are never created. All peers must use this lock.
func MaintainStore(ctx context.Context, dir string, policy Policy) StoreAudit {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report := newStoreAudit(dir)
	if err := policy.Validate(); err != nil {
		report.fail(err.Error())
		return report
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		report.Status = StoreMissing
		return report
	}
	if err != nil || !info.IsDir() {
		report.fail("namespace is not a readable directory")
		return report
	}
	logger := log.New()
	logger.SetLevel(log.InfoLevel)
	h := &Handler{dir: dir, storage: &Storage{rootDir: filepath.Join(dir, "cache")}, policy: policy, logger: logger}
	lock, err := h.transferLock(false)
	if err != nil {
		report.fail("transfer coordination unavailable: " + err.Error())
		if errors.Is(err, boltErrors.ErrTimeout) {
			report.Status = StoreBusy
		}
		return report
	}
	defer lock.Close()
	db, err := openExistingMaintenanceDB(dir)
	if err != nil {
		report.fail("metadata unavailable: " + err.Error())
		return report
	}
	defer db.Close()
	before := auditInventory(ctx, h, db, 0, true)
	if before.Partial {
		return before
	}
	if err := ctx.Err(); err != nil {
		before.fail(err.Error())
		return before
	}
	retention := &RetentionReport{BudgetBytes: policy.MaxBytes}
	collectErr := h.collect(ctx, db, retention)
	after := auditLocked(ctx, h, db, 0)
	after.Retention = retention
	if collectErr != nil {
		after.fail("retention: " + collectErr.Error())
	}
	return after
}

func openExistingMaintenanceDB(dir string) (*bolthold.Store, error) {
	path := filepath.Join(dir, "bolt.db")
	// #nosec G703 -- explicit caller-selected local metadata file.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > auditMaxMetadataBytes {
		return nil, os.ErrInvalid
	}
	return bolthold.Open(path, 0o600, &bolthold.Options{Encoder: json.Marshal, Decoder: json.Unmarshal,
		Options: &bbolt.Options{Timeout: 25 * time.Millisecond, OpenFile: openExistingWritableFile}})
}

func openExistingWritableFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags&^os.O_CREATE, mode)
}
