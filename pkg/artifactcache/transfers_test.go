package artifactcache

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pausedCacheWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pausedCacheWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(p)
}

func TestGCProtectsTransferFromAnotherServer(t *testing.T) {
	dir := t.TempDir()
	reader, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	collector, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, collector.Close()) })
	cache := &Cache{Key: "warm", Version: "v", Complete: true, Size: 80,
		CreatedAt: time.Now().Add(-31 * 24 * time.Hour).Unix(),
		UsedAt:    time.Now().Add(-time.Hour).Unix()}
	db, err := reader.openDB()
	require.NoError(t, err)
	require.NoError(t, insertCache(db, cache))
	require.NoError(t, db.Close())
	archive := reader.storage.filename(cache.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0o755))
	require.NoError(t, os.WriteFile(archive, make([]byte, 80), 0o600))
	writer := &pausedCacheWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("GET", fmt.Sprintf("/%s%s/artifacts/%d", reader.token, apiPath, cache.ID), nil)
		reader.router.ServeHTTP(writer, req)
	}()
	defer func() { close(writer.release); <-done }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive download did not start")
	}
	// Model a transfer lasting longer than the five-minute recent-use grace.
	db, err = reader.openDB()
	require.NoError(t, err)
	require.NoError(t, db.Update(cache.ID, cache))
	require.NoError(t, db.Close())
	collector.gcAt = time.Time{}
	collector.gcCache()
	_, err = os.Stat(archive)
	require.NoError(t, err, "another server must not unlink an archive during a transfer")
}

func TestTransferLockCannotCreateAnAbsentNamespace(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{dir: dir}
	lock, err := h.transferLock(false)
	if lock != nil {
		lock.Close()
	}
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, "transfers.bolt"))
	require.ErrorIs(t, err, os.ErrNotExist, "inspection must never create a cache namespace or coordination file")
}

func TestTransferLeasesPreserveReadOnlyFileAndExcludePeers(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{dir: dir}
	require.NoError(t, h.prepareTransferLock())
	path := filepath.Join(dir, "transfers.bolt")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0400))
	defer func() { require.NoError(t, os.Chmod(path, 0600)) }()
	first, err := openTransferLease(path, true, 25*time.Millisecond)
	require.NoError(t, err)
	second, err := openTransferLease(path, true, 25*time.Millisecond)
	require.NoError(t, err)
	blocked, err := openTransferLease(path, false, 25*time.Millisecond)
	require.Error(t, err)
	require.Nil(t, blocked)
	require.NoError(t, first.Close())
	blocked, err = openTransferLease(path, false, 25*time.Millisecond)
	require.Error(t, err)
	require.Nil(t, blocked)
	require.NoError(t, second.Close())
	exclusive, err := openTransferLease(path, false, 25*time.Millisecond)
	require.NoError(t, err)
	reader, err := openTransferLease(path, true, 25*time.Millisecond)
	require.Error(t, err)
	require.Nil(t, reader)
	require.NoError(t, exclusive.Close())
	require.NoError(t, exclusive.Close(), "lease close is idempotent")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "coordination after initialization must never mutate the file")
}
