package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seedCohortStore(t *testing.T, root, name string, n int) *Handler {
	t.Helper()
	policy := DefaultPolicy()
	policy.CohortRoot = root
	h, err := StartHandlerWithPolicy(filepath.Join(root, name), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	seedAuditEntries(t, h, n)
	require.NoError(t, h.Close())
	return h
}

func TestAggregateBudgetRetainsNewestAcrossNamespaces(t *testing.T) {
	root := t.TempDir()
	seedCohortStore(t, root, "a", 2)
	h := seedCohortStore(t, root, "b", 2)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []*Cache
	require.NoError(t, db.Find(&caches, nil))
	for _, cache := range caches {
		cache.UsedAt = time.Now().Add(-10 * time.Minute).Unix()
		require.NoError(t, db.Update(cache.ID, cache))
	}
	require.NoError(t, db.Close())
	policy := DefaultPolicy()
	policy.MaxBytes = 160
	// Each namespace is individually within budget: namespace GC alone leaves 320 bytes.
	for _, name := range []string{"a", "b"} {
		before := MaintainStore(context.Background(), filepath.Join(root, name), policy)
		require.False(t, before.Partial, before.Errors)
		require.EqualValues(t, 160, *before.ArchiveBytes)
	}
	report := MaintainCohort(context.Background(), root, 160, policy)
	require.False(t, report.Partial, report.Errors)
	require.True(t, *report.BudgetMet)
	require.EqualValues(t, 160, *report.RemainingCompletedBytes)
	require.Len(t, report.Namespaces, 2)
	require.EqualValues(t, 0, *report.Namespaces[0].ArchiveBytes)
	require.EqualValues(t, 160, *report.Namespaces[1].ArchiveBytes)
	require.EqualValues(t, 2, report.Namespaces[0].Retention.DeletedCount)
	require.Equal(t, EvictionAggregate, report.Namespaces[0].Retention.Receipts[0].Reason)
}

func TestAggregateRefusesPartialNamespaceBeforeAnyDeletion(t *testing.T) {
	root := t.TempDir()
	seedCohortStore(t, root, "a", 2)
	seedCohortStore(t, root, "z", 2)
	require.NoError(t, os.WriteFile(filepath.Join(root, "z", "cache", "unknown"), nil, 0o600))
	report := MaintainCohort(context.Background(), root, 1, DefaultPolicy())
	require.True(t, report.Partial)
	require.Nil(t, report.RemainingCompletedBytes)
	require.EqualValues(t, 160, *AuditStore(context.Background(), filepath.Join(root, "a"), 0).ArchiveBytes)
}

func TestAggregateRefusesActiveCohortAndDoesNotCreateAbsentRoot(t *testing.T) {
	root := t.TempDir()
	policy := DefaultPolicy()
	policy.CohortRoot = root
	h, err := StartHandlerWithPolicy(filepath.Join(root, "a"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	report := MaintainCohort(context.Background(), root, 1, policy)
	require.True(t, report.Partial)
	require.Nil(t, report.BudgetMet)
	absent := filepath.Join(root, "absent")
	report = MaintainCohort(context.Background(), absent, 1, policy)
	require.True(t, report.Partial)
	_, err = os.Stat(absent)
	require.True(t, os.IsNotExist(err))
}

func TestAggregateReportsRecentlyUsedProtectedBytes(t *testing.T) {
	root := t.TempDir()
	h := seedCohortStore(t, root, "a", 2)
	db, err := h.openDB()
	require.NoError(t, err)
	var caches []*Cache
	require.NoError(t, db.Find(&caches, nil))
	for _, cache := range caches {
		cache.UsedAt = time.Now().Unix()
		require.NoError(t, db.Update(cache.ID, cache))
	}
	require.NoError(t, db.Close())
	report := MaintainCohort(context.Background(), root, 100, DefaultPolicy())
	require.False(t, report.Partial, report.Errors)
	require.False(t, *report.BudgetMet)
	require.EqualValues(t, 160, *report.RemainingCompletedBytes)
	require.EqualValues(t, 160, *report.ProtectedBytes)
	require.Zero(t, report.Namespaces[0].Retention.DeletedCount)
}
