package artifactcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
)

func TestIdleServerExpiresOverBudgetArchives(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxBytes = 100
	policy.GCInterval = 50 * time.Millisecond
	h, err := StartHandlerWithPolicy(t.TempDir(), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	now := time.Now()
	old := &Cache{Key: "old", Version: "v", Complete: true, Size: 1,
		CreatedAt: now.Add(-3 * time.Hour).Unix(), UsedAt: now.Add(-2 * time.Hour).Unix()}
	warm := &Cache{Key: "warm", Version: "v", Complete: true, Size: 1,
		CreatedAt: now.Add(-3 * time.Hour).Unix(), UsedAt: now.Add(-time.Hour).Unix()}
	db, err := h.openDB()
	require.NoError(t, err)
	for _, c := range []*Cache{old, warm} {
		require.NoError(t, insertCache(db, c))
		file := h.storage.filename(c.ID)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, make([]byte, 80), 0o600))
	}
	require.NoError(t, db.Close())
	// No request and no direct GC invocation: periodic idle maintenance must run.
	require.Eventually(t, func() bool {
		_, err := os.Stat(h.storage.filename(old.ID))
		return os.IsNotExist(err)
	}, 5*time.Second, 25*time.Millisecond)
	db, err = h.openDB()
	require.NoError(t, err)
	defer db.Close()
	require.ErrorIs(t, db.Get(old.ID, &Cache{}), bolthold.ErrNotFound)
	require.NoError(t, db.Get(warm.ID, &Cache{}))
	_, err = os.Stat(h.storage.filename(warm.ID))
	require.NoError(t, err, "quota must preserve the newest warm archive")
}

func TestInvalidRetentionPolicyRejectedBeforeStoreCreation(t *testing.T) {
	for _, name := range []string{"negative-bytes", "negative-cohort-bytes", "cohort-without-root", "zero-age", "zero-unused", "zero-interval"} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultPolicy()
			switch name {
			case "negative-bytes":
				policy.MaxBytes = -1
			case "negative-cohort-bytes":
				policy.CohortMaxBytes = -1
			case "cohort-without-root":
				policy.CohortMaxBytes = 100
			case "zero-age":
				policy.MaxAge = 0
			case "zero-unused":
				policy.UnusedAge = 0
			case "zero-interval":
				policy.GCInterval = 0
			}
			dir := filepath.Join(t.TempDir(), "store")
			h, err := StartHandlerWithPolicy(dir, "", "127.0.0.1", 0, nil, policy)
			require.Error(t, err)
			require.Nil(t, h)
			_, err = os.Stat(dir)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestDeletionFailureKeepsMetadataForRetry(t *testing.T) {
	h, err := StartHandler(t.TempDir(), "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	defer h.Close()
	db, err := h.openDB()
	require.NoError(t, err)
	defer db.Close()
	cache := &Cache{Key: "retained", Version: "v", Complete: true}
	require.NoError(t, insertCache(db, cache))
	archive := h.storage.filename(cache.ID)
	// A nonempty directory cannot be removed as an archive, even as root.
	require.NoError(t, os.MkdirAll(archive, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archive, "unexpected"), []byte("data"), 0o600))
	report := &RetentionReport{}
	require.Error(t, h.deleteCache(db, cache, EvictionBudget, report))
	require.Zero(t, report.DeletedCount)
	require.Zero(t, report.ReclaimedArchiveBytes)
	require.Empty(t, report.Receipts)
	require.NoError(t, db.Get(cache.ID, &Cache{}), "failed filesystem deletion must not erase its accounting record")
}
