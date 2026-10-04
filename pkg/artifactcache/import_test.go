package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedLegacyImportStore(t *testing.T, n int) *Handler {
	t.Helper()
	dir := t.TempDir()
	storage, err := NewStorage(filepath.Join(dir, "cache"))
	require.NoError(t, err)
	h := &Handler{dir: dir, storage: storage}
	seedAuditEntries(t, h, n)
	return h
}

func TestImportPreservesLegacyDataAndWarmHit(t *testing.T) {
	source := seedLegacyImportStore(t, 2)
	before, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "cohort")
	report := ImportCompleted(context.Background(), source.dir, root, "repo", 160)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	require.Empty(t, report.PendingStage)
	require.EqualValues(t, 2, report.ImportedCount)
	require.EqualValues(t, 160, report.ImportedBytes)
	require.EqualValues(t, 160, *report.RetainedSourceArchiveBytes)
	require.NotNil(t, report.AvailableDestinationBytes)
	require.GreaterOrEqual(t, *report.AvailableDestinationBytes, *report.RequiredAdditionalBytes)
	require.Len(t, report.Receipts, 2)
	sum := sha256.Sum256(make([]byte, 80))
	require.Equal(t, hex.EncodeToString(sum[:]), report.Receipts[0].SHA256)
	after, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	require.Equal(t, before, after, "legacy metadata and retention timestamps must not change")
	_, err = os.Stat(filepath.Join(source.dir, "transfers.bolt"))
	require.True(t, os.IsNotExist(err), "legacy source must not be enrolled or changed")
	policy := DefaultPolicy()
	policy.CohortRoot = root
	h, err := StartHandlerWithPolicy(report.Destination, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	assertArchiveHit(t, h, "warm-1")
	db, err := h.openDB()
	require.NoError(t, err)
	cache := &Cache{Key: "new", Version: "v"}
	require.NoError(t, insertCache(db, cache))
	require.Greater(t, cache.ID, report.Receipts[1].DestinationID, "future reservations must not collide with imported IDs")
	require.NoError(t, db.Close())
}

func TestImportHonorsByteBoundAndRefusesOverwrite(t *testing.T) {
	source := seedLegacyImportStore(t, 2)
	root := t.TempDir()
	report := ImportCompleted(context.Background(), source.dir, root, "repo", 80)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	require.EqualValues(t, 1, report.ImportedCount)
	require.EqualValues(t, 1, report.SkippedBudget)
	second := ImportCompleted(context.Background(), source.dir, root, "repo", 160)
	require.True(t, second.Partial)
	require.False(t, second.Published)
	require.Empty(t, second.PendingStage)
	require.EqualValues(t, 80, *AuditStore(context.Background(), report.Destination, 0).ArchiveBytes)
}

func TestImportRejectsMissingCorruptAndCancelledWithoutPublishing(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := ImportCompleted(ctx, source.dir, root, "cancelled", 80)
	require.True(t, report.Partial)
	require.False(t, report.Published)
	require.Empty(t, report.PendingStage)
	// Completed metadata length must agree with the actual archive.
	require.NoError(t, os.WriteFile(source.storage.filename(1), []byte("short"), 0o600))
	report = ImportCompleted(context.Background(), source.dir, root, "corrupt", 80)
	require.True(t, report.Partial)
	require.False(t, report.Published)
	_, err := os.Stat(filepath.Join(root, "corrupt"))
	require.True(t, os.IsNotExist(err))
	report = ImportCompleted(context.Background(), filepath.Join(root, "absent"), root, "missing", 80)
	require.True(t, report.Partial)
	require.False(t, report.Published)
}

func TestImportChangedArchiveNeverPublishesAndCleansStage(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	db, err := openAuditDB(source.dir)
	require.NoError(t, err)
	defer db.Close()
	inventory := auditLocked(context.Background(), source, db, 0)
	cache := &Cache{}
	require.NoError(t, db.Get(uint64(1), cache))
	// Simulate a legacy file mutation after inventory, outside metadata locking.
	require.NoError(t, os.WriteFile(source.storage.filename(1), []byte("changed"), 0o600))
	root := t.TempDir()
	report := ImportReport{Source: source.dir, Destination: filepath.Join(root, "repo")}
	err = publishImport(context.Background(), source, db, inventory.Fingerprint, root, 80, []*Cache{cache}, &report)
	require.ErrorContains(t, err, "length")
	require.False(t, report.Published)
	require.Empty(t, report.PendingStage)
	require.Zero(t, report.ImportedCount)
	require.Empty(t, report.Receipts)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed staging must leave only the cohort coordination file")
	require.Equal(t, "transfers.bolt", entries[0].Name())
	data, err := os.ReadFile(source.storage.filename(1))
	require.NoError(t, err)
	require.Equal(t, "changed", string(data), "cleanup must never remove source data")
}

func TestImportRefusesColdCutoverWhenNoCompletedArchiveFits(t *testing.T) {
	source := seedLegacyImportStore(t, 2)
	before, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "cohort")
	report := ImportCompleted(context.Background(), source.dir, root, "repo", 79)
	require.True(t, report.Partial, "a warm cutover must not publish an empty store")
	require.False(t, report.Published)
	require.EqualValues(t, 2, report.SkippedBudget)
	require.Empty(t, report.PendingStage)
	_, err = os.Stat(report.Destination)
	require.True(t, os.IsNotExist(err))
	after, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Refusal must leave the destination available for a warmer retry.
	retry := ImportCompleted(context.Background(), source.dir, root, "repo", 80)
	require.False(t, retry.Partial, retry.Error)
	require.True(t, retry.Published)
	require.EqualValues(t, 1, retry.ImportedCount)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	h, err := StartHandlerWithPolicy(retry.Destination, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	assertArchiveHit(t, h, "warm-0")
}

func TestImportAllowsGenuinelyEmptySource(t *testing.T) {
	source := seedLegacyImportStore(t, 0)
	report := ImportCompleted(context.Background(), source.dir, t.TempDir(), "repo", 80)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	require.Zero(t, report.ImportedCount)
	require.Zero(t, report.SkippedBudget)
}

func TestImportPublishesRecoveryReceiptWithNamespace(t *testing.T) {
	source := seedLegacyImportStore(t, 2)
	root := t.TempDir()
	report := ImportCompleted(context.Background(), source.dir, root, "repo", 80)
	require.False(t, report.Partial, report.Error)
	data, err := os.ReadFile(filepath.Join(report.Destination, "import-receipt-v1.json"))
	require.NoError(t, err, "a lost stdout acknowledgement needs evidence in the published namespace")
	require.NotEmpty(t, data)
}
