package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
	boltErrors "go.etcd.io/bbolt/errors"
)

const auditPageSize = 12
const auditMaxFiles = 100000
const auditMaxMetadataBytes = 64 * 1024 * 1024

type StoreStatus string

const (
	StoreMissing StoreStatus = "missing"
	StoreReady   StoreStatus = "ready"
	StoreBusy    StoreStatus = "busy"
	StorePartial StoreStatus = "partial"
)

// StoreAudit is a consistent catalog of one namespace, with bounded pages.
// ArchiveBytes counts apparent file lengths, including temporary/untracked
// files. Missing or unreadable data is never represented as a clean zero.
type StoreAudit struct {
	Retention      *RetentionReport `json:"retention,omitempty"`
	SchemaVersion  int              `json:"schema_version"`
	Namespace      string           `json:"namespace"`
	Status         StoreStatus      `json:"status"`
	Partial        bool             `json:"partial"`
	Errors         []string         `json:"errors"`
	EntryCount     *uint64          `json:"entry_count"`
	ArchiveBytes   *int64           `json:"archive_bytes"`
	TemporaryBytes int64            `json:"temporary_bytes"`
	UntrackedBytes int64            `json:"untracked_bytes"`
	Fingerprint    string           `json:"fingerprint"`
	Entries        []StoreEntry     `json:"entries"`
	NextCursor     *uint64          `json:"next_cursor"`
}

type StoreEntry struct {
	Cache
	Bytes *int64 `json:"bytes"`
}

// AuditStore does not create or change cache data. It takes the exclusive
// transfer lock through a read-only descriptor and opens metadata read-only.
// Cursor pages must have matching fingerprints to constitute one full audit.
// Legacy namespaces without transfer coordination are reported as partial.
func AuditStore(ctx context.Context, dir string, cursor uint64) StoreAudit {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report := newStoreAudit(dir)
	h := &Handler{dir: dir, storage: &Storage{rootDir: filepath.Join(dir, "cache")}}
	info, err := os.Lstat(dir) // #nosec G703 -- explicit caller-selected namespace; no restricted-root API.
	if os.IsNotExist(err) {
		report.Status = StoreMissing
		return report
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		report.fail("cache namespace is not a readable directory")
		return report
	}
	lock, err := h.transferLock(false)
	if err != nil {
		report.fail("transfer coordination unavailable: " + err.Error())
		if errors.Is(err, boltErrors.ErrTimeout) {
			report.Status = StoreBusy
		}
		return report
	}
	defer lock.Close()
	db, err := openAuditDB(dir)
	if err != nil {
		report.fail("metadata unavailable: " + err.Error())
		return report
	}
	defer db.Close()
	return auditLocked(ctx, h, db, cursor)
}

func newStoreAudit(dir string) StoreAudit {
	return StoreAudit{SchemaVersion: 1, Namespace: filepath.Base(filepath.Clean(dir)),
		Status: StoreReady, Entries: []StoreEntry{}, Errors: []string{}}
}

func (a *StoreAudit) fail(detail string) {
	a.Partial = true
	a.Status = StorePartial
	if len(a.Errors) < 8 {
		text := []rune(detail)
		if len(text) > 128 {
			text = text[:128]
		}
		a.Errors = append(a.Errors, string(text))
	}
	a.Fingerprint = ""
}

func openAuditDB(dir string) (*bolthold.Store, error) {
	path := filepath.Join(dir, "bolt.db")
	info, err := os.Lstat(path) // #nosec G703 -- metadata inside the explicit caller-selected namespace.
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > auditMaxMetadataBytes {
		return nil, fmt.Errorf("metadata is not a regular bounded file")
	}
	return bolthold.Open(path, 0o600, &bolthold.Options{Encoder: json.Marshal, Decoder: json.Unmarshal,
		Options: &bbolt.Options{ReadOnly: true, Timeout: 25 * time.Millisecond}})
}

func auditLocked(ctx context.Context, h *Handler, db *bolthold.Store, cursor uint64) StoreAudit {
	report := newStoreAudit(h.dir)
	files, err := archiveFiles(ctx, h.storage.rootDir)
	if err != nil {
		report.fail("archive inventory: " + err.Error())
		return report
	}
	var bytes int64
	for _, size := range files {
		if size > math.MaxInt64-bytes {
			report.fail("archive size overflow")
			return report
		}
		bytes += size
	}
	report.ArchiveBytes = &bytes
	hash := sha256.New()
	var count uint64
	var page []StoreEntry
	err = db.ForEach(nil, func(cache *Cache) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > auditMaxFiles {
			return fmt.Errorf("cache metadata entry limit exceeded")
		}
		if len(cache.Key) > 512 || len(cache.Version) > 128 {
			return fmt.Errorf("cache identity exceeds audit bounds")
		}
		encoded, err := json.Marshal(cache)
		if err != nil {
			return err
		}
		hash.Write(encoded)
		entry := StoreEntry{Cache: *cache}
		if cache.Complete {
			name := h.storage.filename(cache.ID)
			size, ok := files[name]
			if !ok {
				report.fail(fmt.Sprintf("cache %d archive is missing", cache.ID))
			} else {
				entry.Bytes = &size
				delete(files, name)
			}
		}
		encoded, err = json.Marshal(entry.Bytes)
		if err != nil {
			return err
		}
		hash.Write(encoded)
		page = includeAuditEntry(page, entry, cursor)
		return nil
	})
	if err != nil {
		report.fail("metadata inventory: " + err.Error())
		return report
	}
	report.EntryCount = &count
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	temporary := filepath.Join(h.storage.rootDir, "tmp") + string(os.PathSeparator)
	untracked := false
	for _, name := range names {
		size := files[name]
		hash.Write([]byte(name))
		hash.Write([]byte(fmt.Sprint(size)))
		if strings.HasPrefix(name, temporary) {
			report.TemporaryBytes += size
		} else {
			report.UntrackedBytes += size
			untracked = true
		}
	}
	if untracked {
		report.fail("untracked archive files require reconciliation")
	}
	if !report.Partial {
		report.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	}
	if len(page) > auditPageSize {
		next := page[auditPageSize-1].ID
		report.NextCursor = &next
		page = page[:auditPageSize]
	}
	report.Entries = page
	return report
}

func archiveFiles(ctx context.Context, root string) (map[string]int64, error) {
	files := make(map[string]int64)
	var count int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > auditMaxFiles {
			return fmt.Errorf("archive inventory limit exceeded")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular archive file")
		}
		files[path] = info.Size()
		return nil
	})
	return files, err
}

func includeAuditEntry(page []StoreEntry, entry StoreEntry, cursor uint64) []StoreEntry {
	if entry.ID <= cursor {
		return page
	}
	page = append(page, entry)
	sort.Slice(page, func(i, j int) bool { return page[i].ID < page[j].ID })
	if len(page) > auditPageSize+1 {
		page = page[:auditPageSize+1]
	}
	return page
}
