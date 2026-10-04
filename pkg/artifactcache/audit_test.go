package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seedAuditStore(t *testing.T, dir string, n int) *Handler {
	t.Helper()
	h, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	db, err := h.openDB()
	require.NoError(t, err)
	now := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < n; i++ {
		cache := &Cache{Key: fmt.Sprintf("warm-%d", i), Version: "v", Complete: true, Size: 80, CreatedAt: now, UsedAt: now}
		require.NoError(t, insertCache(db, cache))
		name := h.storage.filename(cache.ID)
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
		require.NoError(t, os.WriteFile(name, make([]byte, 80), 0o600))
	}
	require.NoError(t, db.Close())
	require.NoError(t, h.Close())
	return h
}

func TestAuditPagesAreConsistentAndDoNotChangeStore(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 30)
	before, err := os.ReadFile(filepath.Join(dir, "bolt.db")) // #nosec G703 -- disposable fixture metadata.
	require.NoError(t, err)
	var cursor uint64
	var fingerprint string
	var ids []uint64
	for {
		report := AuditStore(context.Background(), dir, cursor)
		require.Equal(t, StoreReady, report.Status, report.Errors)
		require.False(t, report.Partial)
		require.EqualValues(t, 30, *report.EntryCount)
		require.EqualValues(t, 2400, *report.ArchiveBytes)
		if fingerprint == "" {
			fingerprint = report.Fingerprint
		} else {
			require.Equal(t, fingerprint, report.Fingerprint)
		}
		for _, entry := range report.Entries {
			ids = append(ids, entry.ID)
			require.EqualValues(t, 80, *entry.Bytes)
		}
		encoded, err := json.Marshal(report)
		require.NoError(t, err)
		require.Less(t, len(encoded), 64*1024)
		if report.NextCursor == nil {
			break
		}
		cursor = *report.NextCursor
	}
	require.Len(t, ids, 30)
	for i := 1; i < len(ids); i++ {
		require.Greater(t, ids[i], ids[i-1])
	}
	after, err := os.ReadFile(filepath.Join(dir, "bolt.db")) // #nosec G703 -- disposable fixture metadata.
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(before), sha256.Sum256(after))
	// A writer between pages must invalidate the cohort fingerprint.
	db, err := h.openDB()
	require.NoError(t, err)
	cache := &Cache{}
	require.NoError(t, db.Get(ids[0], cache))
	cache.UsedAt++
	require.NoError(t, db.Update(cache.ID, cache))
	require.NoError(t, db.Close())
	require.NotEqual(t, fingerprint, AuditStore(context.Background(), dir, 0).Fingerprint)
}

func TestAuditMissingBusyAndCorruptAreNotEmpty(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		report := AuditStore(context.Background(), dir, 0)
		require.Equal(t, StoreMissing, report.Status)
		require.Nil(t, report.ArchiveBytes)
		_, err := os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("busy", func(t *testing.T) {
		dir := t.TempDir()
		h := seedAuditStore(t, dir, 1)
		reader, err := h.transferLock(true)
		require.NoError(t, err)
		defer reader.Close()
		report := AuditStore(context.Background(), dir, 0)
		require.Equal(t, StoreBusy, report.Status)
		require.True(t, report.Partial)
		require.Nil(t, report.EntryCount)
		require.Nil(t, report.ArchiveBytes)
	})
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		seedAuditStore(t, dir, 1)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bolt.db"), []byte("corrupt"), 0o600))
		report := AuditStore(context.Background(), dir, 0)
		require.Equal(t, StorePartial, report.Status)
		require.True(t, report.Partial)
		require.Nil(t, report.EntryCount)
		require.Nil(t, report.ArchiveBytes)
	})
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		seedAuditStore(t, dir, 0)
		report := AuditStore(context.Background(), dir, 0)
		require.Equal(t, StoreReady, report.Status, report.Errors)
		require.False(t, report.Partial)
		require.EqualValues(t, 0, *report.ArchiveBytes)
		require.EqualValues(t, 0, *report.EntryCount)
	})
}

func TestAuditMissingArchiveAndUntrackedFilesRemainVisible(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 1)
	// Delete only the synthetic test archive, then leave an untracked archive.
	require.NoError(t, h.storage.Remove(1))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cache", "untracked"), make([]byte, 70), 0o600))
	report := AuditStore(context.Background(), dir, 0)
	require.True(t, report.Partial)
	require.Nil(t, report.Entries[0].Bytes)
	require.EqualValues(t, 70, *report.ArchiveBytes)
	require.EqualValues(t, 70, report.UntrackedBytes)
	require.Empty(t, report.Fingerprint)
}

// A compiled test binary can seed a disposable Docker volume, then inspect the
// same volume mounted read-only. The write probe verifies the mount, even as root.
func TestAuditReadOnlyMountedFixture(t *testing.T) {
	dir := os.Getenv("ACT2_AUDIT_FIXTURE_PATH")
	if dir == "" {
		t.Skip("requires an external disposable-volume harness")
	}
	if os.Getenv("ACT2_AUDIT_FIXTURE_SEED") == "1" {
		seedAuditStore(t, dir, 2)
		return
	}
	err := os.WriteFile(filepath.Join(dir, "write-probe"), []byte("probe"), 0o600) // #nosec G703 -- disposable harness fixture explicitly selected by its parent.
	require.ErrorIs(t, err, syscall.EROFS, "fixture must actually be mounted read-only")
	before, err := os.ReadFile(filepath.Join(dir, "bolt.db")) // #nosec G703 -- disposable fixture metadata.
	require.NoError(t, err)
	report := AuditStore(context.Background(), dir, 0)
	require.False(t, report.Partial, report.Errors)
	require.Equal(t, StoreReady, report.Status)
	require.EqualValues(t, 160, *report.ArchiveBytes)
	require.EqualValues(t, 2, *report.EntryCount)
	after, err := os.ReadFile(filepath.Join(dir, "bolt.db")) // #nosec G703 -- disposable fixture metadata.
	require.NoError(t, err)
	require.Equal(t, before, after)
}
