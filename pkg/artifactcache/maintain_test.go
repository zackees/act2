package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOfflineMaintenanceBoundsArchivesWithoutStartingServer(t *testing.T) {
	dir := t.TempDir()
	seedAuditStore(t, dir, 2) // The server is already closed.
	before := AuditStore(context.Background(), dir, 0)
	require.EqualValues(t, 160, *before.ArchiveBytes)
	policy := DefaultPolicy()
	policy.MaxBytes = 100
	after := MaintainStore(context.Background(), dir, policy)
	require.False(t, after.Partial, after.Errors)
	require.EqualValues(t, 80, *after.ArchiveBytes, "the offline namespace must obey its byte ceiling")
	require.EqualValues(t, 1, after.Retention.DeletedCount)
	require.EqualValues(t, 80, after.Retention.ReclaimedArchiveBytes)
	require.True(t, *after.Retention.BudgetMet)
	require.Equal(t, EvictionBudget, after.Retention.Receipts[0].Reason)
	require.Len(t, after.Entries, 1)
	require.Equal(t, "warm-1", after.Entries[0].Key, "the newest cache remains warm")
}

func TestOfflineMaintenanceRefusesUntrackedFiles(t *testing.T) {
	dir := t.TempDir()
	seedAuditStore(t, dir, 2)
	unknown := filepath.Join(dir, "cache", "unknown")
	require.NoError(t, os.WriteFile(unknown, []byte("foreign"), 0o600))
	policy := DefaultPolicy()
	policy.MaxBytes = 1
	after := MaintainStore(context.Background(), dir, policy)
	require.True(t, after.Partial)
	require.EqualValues(t, 167, *after.ArchiveBytes)
	require.Len(t, after.Entries, 2, "unknown data must prevent eviction")
	data, err := os.ReadFile(unknown)
	require.NoError(t, err)
	require.Equal(t, "foreign", string(data))
}

func TestOfflineMaintenanceRejectsPolicyBeforeCreatingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	policy := DefaultPolicy()
	policy.MaxBytes = -1
	report := MaintainStore(context.Background(), dir, policy)
	require.True(t, report.Partial)
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err))
}

func TestOfflineMaintenanceReportsProtectedOverBudget(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 2)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []*Cache
	require.NoError(t, db.Find(&caches, nil))
	for _, cache := range caches {
		cache.UsedAt = time.Now().Unix()
		require.NoError(t, db.Update(cache.ID, cache))
	}
	require.NoError(t, db.Close())
	policy := DefaultPolicy()
	policy.MaxBytes = 100
	report := MaintainStore(context.Background(), dir, policy)
	require.False(t, report.Partial, report.Errors)
	require.False(t, *report.Retention.BudgetMet)
	require.EqualValues(t, 160, *report.Retention.ProtectedBytes)
	require.EqualValues(t, 160, *report.Retention.RemainingCompletedBytes)
	require.Zero(t, report.Retention.DeletedCount)
	require.Empty(t, report.Retention.Receipts)
}

func TestOfflineMaintenanceRefusesEmptyUntrackedFile(t *testing.T) {
	dir := t.TempDir()
	seedAuditStore(t, dir, 2)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cache", "unknown-empty"), nil, 0o600))
	policy := DefaultPolicy()
	policy.MaxBytes = 1
	report := MaintainStore(context.Background(), dir, policy)
	require.True(t, report.Partial, "unknown zero-byte files still invalidate inventory")
	require.EqualValues(t, 160, *report.ArchiveBytes)
	require.Nil(t, report.Retention)
}
