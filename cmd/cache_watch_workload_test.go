package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
	bolt "go.etcd.io/bbolt"
)

// The watcher must collect stored data without relying on a server's shutdown
// callback, then a fresh server must still retrieve the protected warm entry.
func TestCohortWatchCollectsIdleArchivesAndPreservesFreshServerHit(t *testing.T) {
	dir := t.TempDir()
	policy := artifactcache.DefaultPolicy()
	policy.CohortRoot = dir
	seedWatcherNamespace(t, dir, "cold", 2, time.Now().Add(-time.Hour).Unix())
	seedWatcherNamespace(t, dir, "warm", 1, time.Now().Unix())
	before := artifactcache.MaintainCohort(context.Background(), dir, 0, policy)
	require.False(t, before.Partial, before.Errors)
	require.EqualValues(t, 240, *before.RemainingCompletedBytes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := createRootCommand(ctx, &Input{}, "test")
	output := &reportCallback{after: func(data []byte) {
		var report artifactcache.CohortReport
		require.NoError(t, json.Unmarshal(data, &report))
		require.False(t, report.Partial, report.Errors)
		require.True(t, *report.BudgetMet)
		require.EqualValues(t, 80, *report.RemainingCompletedBytes)
		cancel()
	}}
	root.SetOut(output)
	root.SetArgs([]string{"cache", "prune-cohort", "--apply", "--max-bytes", "80", "--watch", "1ms", "--cache-server-path", dir})
	require.NoError(t, root.Execute())
	cold := artifactcache.AuditStore(context.Background(), filepath.Join(dir, "cold"), 0)
	require.False(t, cold.Partial, cold.Errors)
	require.EqualValues(t, 0, *cold.ArchiveBytes)
	h, err := artifactcache.StartHandlerWithPolicy(filepath.Join(dir, "warm"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(h.ExternalURL() + "/_apis/artifactcache/cache?keys=warm-0&version=v")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var hit struct {
		Location string `json:"archiveLocation"`
		Key      string `json:"cacheKey"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&hit))
	require.NoError(t, response.Body.Close())
	require.Equal(t, "warm-0", hit.Key)
	response, err = client.Get(hit.Location)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, bytes.Repeat([]byte("w"), 80), data)
}

func seedWatcherNamespace(t *testing.T, root, name string, count int, usedAt int64) {
	t.Helper()
	dir := filepath.Join(root, name)
	policy := artifactcache.DefaultPolicy()
	policy.CohortRoot = root
	h, err := artifactcache.StartHandlerWithPolicy(dir, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	require.Nil(t, h.RetentionOnClose(), "fixture must not invoke shutdown collection")
	db, err := bolthold.Open(filepath.Join(dir, "bolt.db"), 0600, &bolthold.Options{
		Encoder: json.Marshal, Decoder: json.Unmarshal, Options: &bolt.Options{Timeout: time.Second},
	})
	require.NoError(t, err)
	defer db.Close()
	storage, err := artifactcache.NewStorage(filepath.Join(dir, "cache"))
	require.NoError(t, err)
	for i := 0; i < count; i++ {
		id := uint64(i + 1)
		entry := &artifactcache.Cache{ID: id, Key: name + "-" + string(rune('0'+i)), Version: "v", Complete: true, Size: 80, CreatedAt: time.Now().Add(-time.Hour).Unix(), UsedAt: usedAt}
		require.NoError(t, db.Insert(id, entry))
		require.NoError(t, storage.Write(id, 0, bytes.NewReader(bytes.Repeat([]byte("w"), 80))))
		_, err := storage.Commit(id, 80)
		require.NoError(t, err)
	}
}
