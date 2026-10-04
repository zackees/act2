package artifactcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
)

func TestCacheBudgetKeepsRecentlyUsedArchivesWithinTheByteLimit(t *testing.T) {
	h, err := StartHandler(t.TempDir(), "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	h.policy.MaxBytes = 100
	now := time.Now()
	old := &Cache{Key: "old", Version: "v", Complete: true, Size: 80, CreatedAt: now.Add(-3 * time.Hour).Unix(), UsedAt: now.Add(-2 * time.Hour).Unix()}
	warm := &Cache{Key: "warm", Version: "v", Complete: true, Size: 80, CreatedAt: now.Add(-3 * time.Hour).Unix(), UsedAt: now.Add(-time.Hour).Unix()}
	db, err := h.openDB()
	require.NoError(t, err)
	for _, c := range []*Cache{old, warm} {
		require.NoError(t, insertCache(db, c))
		file := h.storage.filename(c.ID)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, make([]byte, 80), 0o600))
	}
	require.NoError(t, db.Close())
	h.gcAt = time.Time{}
	h.gcCache()
	db, err = h.openDB()
	require.NoError(t, err)
	defer db.Close()
	require.ErrorIs(t, db.Get(old.ID, &Cache{}), bolthold.ErrNotFound, "the least recently used archive must expire under the byte ceiling")
	require.NoError(t, db.Get(warm.ID, &Cache{}), "the next run must still have a warm cache")
	_, err = os.Stat(h.storage.filename(old.ID))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(h.storage.filename(warm.ID))
	require.NoError(t, err)
}
