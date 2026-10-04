package artifactcache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownRetentionDefersActivePeerAndLastCloseRetries(t *testing.T) {
	root := t.TempDir()
	seedCohortStore(t, root, "a", 1)
	seedCohortStore(t, root, "b", 2)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	policy.CohortMaxBytes = 160
	a, err := StartHandlerWithPolicy(filepath.Join(root, "a"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	b, err := StartHandlerWithPolicy(filepath.Join(root, "b"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Nil(t, a.RetentionOnClose())
	require.NoError(t, a.Close())
	require.True(t, a.RetentionOnClose().Partial)
	require.Nil(t, a.RetentionOnClose().BudgetMet)
	assertArchiveHit(t, b, "warm-0")
	require.NoError(t, b.Close())
	report := b.RetentionOnClose()
	require.False(t, report.Partial, report.Errors)
	require.True(t, *report.BudgetMet)
	require.LessOrEqual(t, *report.RemainingCompletedBytes, int64(160))
	require.NoError(t, b.Close())
	require.Same(t, report, b.RetentionOnClose(), "repeated Close must not repeat maintenance")
}

func TestShutdownRetentionReportsProtectedOverflow(t *testing.T) {
	root := t.TempDir()
	h := seedCohortStore(t, root, "warm", 3)
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
	policy.CohortRoot = root
	policy.CohortMaxBytes = 80
	h, err = StartHandlerWithPolicy(h.dir, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	report := h.RetentionOnClose()
	require.False(t, report.Partial, report.Errors)
	require.False(t, *report.BudgetMet)
	require.EqualValues(t, 240, *report.ProtectedBytes)
	require.EqualValues(t, 240, *report.RemainingCompletedBytes)
	require.Zero(t, report.Namespaces[0].Retention.DeletedCount)
}

func TestShutdownRetentionCancellationLeavesDataAndReleasesLease(t *testing.T) {
	root := t.TempDir()
	seedCohortStore(t, root, "a", 2)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	policy.CohortMaxBytes = 80
	h, err := StartHandlerWithPolicy(filepath.Join(root, "a"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, h.CloseContext(ctx))
	require.True(t, h.RetentionOnClose().Partial)
	audit := AuditStore(context.Background(), h.dir, 0)
	require.False(t, audit.Partial, audit.Errors)
	require.EqualValues(t, 160, *audit.ArchiveBytes)
	gate := &Handler{dir: root}
	lease, err := gate.transferLock(false)
	require.NoError(t, err, "cancellation must not retain the server's root lease")
	require.NoError(t, lease.Close())
}
