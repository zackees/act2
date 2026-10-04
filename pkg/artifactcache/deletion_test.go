package artifactcache

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaintenanceRecoversInterruptedDeletion(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 1)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []Cache
	require.NoError(t, db.Find(&caches, nil))
	require.Len(t, caches, 1)
	cache := caches[0]
	require.NoError(t, db.Upsert(cache.ID, &DeletionIntent{Cache: cache, Reason: EvictionBudget}))
	// Reproduce a crash after file removal, before the metadata transaction.
	require.NoError(t, h.storage.Remove(cache.ID))
	require.NoError(t, db.Close())
	require.True(t, AuditStore(context.Background(), dir, 0).Partial)
	lookup := httptest.NewRecorder()
	h.find(lookup, httptest.NewRequest("GET", "/?keys=warm-0&version=v", nil), nil)
	require.Equal(t, 204, lookup.Code)
	first := MaintainStore(context.Background(), dir, DefaultPolicy())
	require.False(t, first.Partial, first.Errors)
	require.EqualValues(t, 1, first.Retention.DeletedCount)
	require.Zero(t, first.Retention.ReclaimedArchiveBytes)
	second := MaintainStore(context.Background(), dir, DefaultPolicy())
	require.False(t, second.Partial, second.Errors)
	require.Zero(t, second.Retention.DeletedCount)
}

func TestMaintenancePreservesMissingArchiveWithoutDeletionIntent(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 1)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []Cache
	require.NoError(t, db.Find(&caches, nil))
	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(h.storage.filename(caches[0].ID)))
	result := MaintainStore(context.Background(), dir, DefaultPolicy())
	require.True(t, result.Partial)
	require.Nil(t, result.Retention)
}

func TestCohortDefersDeletionRecoveryUntilEveryNamespaceIsSafe(t *testing.T) {
	root := t.TempDir()
	h := seedCohortStore(t, root, "a", 1)
	seedCohortStore(t, root, "z", 1)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []Cache
	require.NoError(t, db.Find(&caches, nil))
	cache := caches[0]
	require.NoError(t, db.Upsert(cache.ID, &DeletionIntent{Cache: cache, Reason: EvictionAggregate}))
	require.NoError(t, h.storage.Remove(cache.ID))
	require.NoError(t, db.Close())
	unknown := filepath.Join(root, "z", "cache", "unknown")
	require.NoError(t, os.WriteFile(unknown, nil, 0o600))
	blocked := MaintainCohort(context.Background(), root, 1000, DefaultPolicy())
	require.True(t, blocked.Partial)
	db, err = h.openDB()
	require.NoError(t, err)
	var intent DeletionIntent
	require.NoError(t, db.Get(cache.ID, &intent))
	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(unknown))
	recovered := MaintainCohort(context.Background(), root, 1000, DefaultPolicy())
	require.False(t, recovered.Partial, recovered.Errors)
	require.EqualValues(t, 1, recovered.Namespaces[0].Retention.DeletedCount)
	require.Zero(t, recovered.Namespaces[0].Retention.ReclaimedArchiveBytes)
}
