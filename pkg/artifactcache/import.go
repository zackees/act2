package artifactcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/timshannon/bolthold"
)

type ImportReport struct {
	RetainedSourceArchiveBytes *int64          `json:"retained_source_archive_bytes"`
	AvailableDestinationBytes  *uint64         `json:"available_destination_bytes"`
	RequiredAdditionalBytes    *uint64         `json:"required_additional_bytes"`
	SchemaVersion              int             `json:"schema_version"`
	Source                     string          `json:"source"`
	Destination                string          `json:"destination"`
	Published                  bool            `json:"published"`
	Partial                    bool            `json:"partial"`
	Error                      string          `json:"error,omitempty"`
	PendingStage               string          `json:"pending_stage,omitempty"`
	ImportedCount              uint64          `json:"imported_count"`
	ImportedBytes              int64           `json:"imported_bytes"`
	SkippedIncomplete          uint64          `json:"skipped_incomplete"`
	SkippedBudget              uint64          `json:"skipped_budget"`
	Receipts                   []ImportReceipt `json:"receipts"`
	ReceiptsOmitted            uint64          `json:"receipts_omitted"`
}

type ImportReceipt struct {
	SourceID      uint64 `json:"source_id"`
	DestinationID uint64 `json:"destination_id"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
}

// ImportCompleted copies completed legacy archives into a new cohort namespace.
// The caller must provide a quiescent source: legacy servers should be stopped.
// It never deletes or modifies source data. A read-only metadata lock blocks
// legacy metadata mutation/GC, and copied files are checksummed against source.
// This detects observed file changes; it cannot coordinate arbitrary legacy
// writers outside their metadata protocol. The destination is atomically
// published only after validation, under an exclusive cohort root lease.
func ImportCompleted(ctx context.Context, source, root, name string, maxBytes int64) ImportReport {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	report := ImportReport{SchemaVersion: 1, Source: source, Destination: filepath.Join(root, name)}
	if err := validateImportPaths(source, root, name, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	sourceDB, err := openAuditDB(source)
	if err != nil {
		report.fail(fmt.Errorf("source metadata: %w", err))
		return report
	}
	defer sourceDB.Close()
	sourceHandler := &Handler{dir: source, storage: &Storage{rootDir: filepath.Join(source, "cache")}}
	inventory := auditLocked(ctx, sourceHandler, sourceDB, 0)
	if inventory.Partial {
		report.fail(fmt.Errorf("source inventory incomplete: %v", inventory.Errors))
		return report
	}
	report.RetainedSourceArchiveBytes = inventory.ArchiveBytes
	caches, err := selectImportCaches(ctx, sourceHandler, sourceDB, maxBytes, &report)
	if err != nil {
		report.fail(err)
		return report
	}
	if len(caches) == 0 && report.SkippedBudget > 0 {
		report.fail(fmt.Errorf("no completed archive fits import budget; increase max-bytes to preserve a warm cache"))
		return report
	}
	if err := publishImport(ctx, sourceHandler, sourceDB, inventory.Fingerprint, root, maxBytes, caches, &report); err != nil {
		report.fail(err)
	}
	return report
}

func publishImport(ctx context.Context, source *Handler, sourceDB *bolthold.Store, fingerprint, root string, maxBytes int64, caches []*Cache, report *ImportReport) error {
	return publishImportWithSpace(ctx, source, sourceDB, fingerprint, root, maxBytes, caches, report, availableImportSpace)
}

func publishImportWithSpace(ctx context.Context, source *Handler, sourceDB *bolthold.Store, fingerprint, root string, maxBytes int64, caches []*Cache, report *ImportReport, probe importSpaceProbe) error {
	stage, lease, err := createImportStage(root, report.Destination)
	if err != nil {
		return err
	}
	defer lease.Close()
	report.PendingStage = stage
	defer cleanupImportStage(stage, report)
	if err := initializeCohortNamespace(stage); err != nil {
		return err
	}
	destination := &Handler{dir: stage, logger: log.New()}
	if err := destination.prepareTransferLock(); err != nil {
		return err
	}
	destination.storage, err = NewStorage(filepath.Join(stage, "cache"))
	if err != nil {
		return err
	}
	if err := checkImportHeadroom(stage, caches, report, probe); err != nil {
		return err
	}
	if err := populateImport(ctx, source, destination, caches, report); err != nil {
		return err
	}
	finalSource := auditLocked(ctx, source, sourceDB, 0)
	if finalSource.Partial || finalSource.Fingerprint != fingerprint {
		return fmt.Errorf("source changed during import")
	}
	if err := writeImportPublicationReceipt(stage, fingerprint, maxBytes, report); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published, err := publishImportStage(stage, report.Destination, root)
	report.Published = published
	return err
}

func createImportStage(root, destination string) (string, transferLease, error) {
	// #nosec G703 -- explicit caller-selected cohort root, checked below.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(root) // #nosec G703 -- explicit caller-selected cohort root.
	if err != nil || !info.IsDir() {
		return "", nil, fmt.Errorf("cohort root is not a directory")
	}
	gate := &Handler{dir: root}
	if err := gate.prepareTransferLock(); err != nil {
		return "", nil, err
	}
	lease, err := gate.transferLock(false)
	if err != nil {
		return "", nil, fmt.Errorf("cohort busy: %w", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		_ = lease.Close()
		return "", nil, fmt.Errorf("destination already exists or is unreadable")
	}
	stage, err := os.MkdirTemp(root, ".import-")
	if err != nil {
		_ = lease.Close()
		return "", nil, err
	}
	return stage, lease, nil
}

func cleanupImportStage(stage string, report *ImportReport) {
	if report.Published {
		report.PendingStage = ""
		return
	}
	report.ImportedCount = 0
	report.ImportedBytes = 0
	report.Receipts = nil
	report.ReceiptsOmitted = 0
	// Only this invocation's generated stage is eligible for cleanup. The source
	// and pre-existing destination are never cleanup targets.
	// #nosec G703 -- only this invocation's generated private stage is removed.
	if err := os.RemoveAll(stage); err != nil {
		report.fail(fmt.Errorf("staging cleanup: %w", err))
	} else {
		report.PendingStage = ""
	}
}

func (r *ImportReport) fail(err error) {
	r.Partial = true
	detail := []rune(err.Error())
	if r.Error != "" {
		detail = []rune(r.Error + "; " + err.Error())
	}
	if len(detail) > 256 {
		detail = detail[:256]
	}
	r.Error = string(detail)
}

func validateImportPaths(source, root, name string, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("import requires a positive byte bound")
	}
	if err := validateImportName(name); err != nil {
		return err
	}
	src, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	parent, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(parent, src)
	if err != nil {
		return err
	}
	if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("source must be outside destination cohort")
	}
	info, err := os.Lstat(source) // #nosec G703 -- explicit caller-selected source outside the destination cohort.
	if err != nil || !info.IsDir() {
		return fmt.Errorf("source must be an existing directory")
	}
	return nil
}

func selectImportCaches(ctx context.Context, h *Handler, db *bolthold.Store, maxBytes int64, report *ImportReport) ([]*Cache, error) {
	var caches []*Cache
	if err := db.Find(&caches, nil); err != nil {
		return nil, err
	}
	sort.Slice(caches, func(i, j int) bool {
		if caches[i].UsedAt != caches[j].UsedAt {
			return caches[i].UsedAt > caches[j].UsedAt
		}
		return caches[i].ID < caches[j].ID
	})
	selected := make([]*Cache, 0, len(caches))
	var bytes int64
	for _, cache := range caches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !cache.Complete {
			report.SkippedIncomplete++
			continue
		}
		info, err := os.Lstat(h.storage.filename(cache.ID))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() != cache.Size {
			return nil, fmt.Errorf("source archive %d does not match completed metadata", cache.ID)
		}
		if info.Size() > maxBytes-bytes {
			report.SkippedBudget++
			continue
		}
		selected = append(selected, cache)
		bytes += info.Size()
	}
	return selected, nil
}

func populateImport(ctx context.Context, source, destination *Handler, caches []*Cache, report *ImportReport) error {
	db, err := destination.openDB()
	if err != nil {
		return err
	}
	for _, cache := range caches {
		copied := *cache
		copied.ID = 0
		copied.Complete = false
		if err := insertCache(db, &copied); err != nil {
			_ = db.Close()
			return err
		}
		digest, err := copyVerifiedArchive(ctx, source.storage.filename(cache.ID), destination.storage.filename(copied.ID), cache.Size)
		if err != nil {
			_ = db.Close()
			return err
		}
		copied.Complete = true
		if err := db.Update(copied.ID, &copied); err != nil {
			_ = db.Close()
			return err
		}
		report.ImportedCount++
		report.ImportedBytes += cache.Size
		if len(report.Receipts) < 12 {
			report.Receipts = append(report.Receipts, ImportReceipt{SourceID: cache.ID, DestinationID: copied.ID, Bytes: cache.Size, SHA256: digest})
		} else {
			report.ReceiptsOmitted++
		}
	}
	return db.Close()
}

func validateImportName(name string) error {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid namespace name")
	}
	for _, c := range name {
		if !validImportRune(c) {
			return fmt.Errorf("invalid namespace name")
		}
	}
	return nil
}

func validImportRune(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}
