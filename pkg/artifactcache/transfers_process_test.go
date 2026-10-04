package artifactcache

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
)

// This helper runs in a separate OS process. It pauses an actual download
// handler, proving coordination is not merely an in-process mutex.
func TestCacheTransferProcess(t *testing.T) {
	dir := os.Getenv("ACT2_TEST_TRANSFER_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	h, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	defer h.Close()
	input := bufio.NewScanner(os.Stdin)
	fmt.Println("READY")
	require.True(t, input.Scan())
	var id uint64
	_, err = fmt.Sscan(input.Text(), &id)
	require.NoError(t, err)
	writer := &pausedCacheWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("GET", fmt.Sprintf("/%s%s/artifacts/%d", h.token, apiPath, id), nil)
		h.router.ServeHTTP(writer, req)
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not enter storage")
	}
	fmt.Println("TRANSFERRING")
	input.Scan()
	close(writer.release)
	<-done
	require.Equal(t, 200, writer.Code)
	require.Equal(t, 80, writer.Body.Len())
}

func TestGCProtectsTransferAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	h, err := StartHandler(dir, "", "127.0.0.1", 0, nil)
	require.NoError(t, err)
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCacheTransferProcess$")
	child.Env = append(os.Environ(), "ACT2_TEST_TRANSFER_DIR="+dir)
	in, err := child.StdinPipe()
	require.NoError(t, err)
	out, err := child.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	require.NoError(t, child.Start())
	defer func() { in.Close(); require.NoError(t, child.Wait(), stderr.String()) }()
	lines := bufio.NewScanner(out)
	require.True(t, lines.Scan())
	require.Equal(t, "READY", lines.Text())
	cache := &Cache{Key: "warm", Version: "v", Complete: true, Size: 80,
		CreatedAt: time.Now().Add(-31 * 24 * time.Hour).Unix(), UsedAt: time.Now().Add(-time.Hour).Unix()}
	db, err := h.openDB()
	require.NoError(t, err)
	require.NoError(t, insertCache(db, cache))
	require.NoError(t, db.Close())
	archive := h.storage.filename(cache.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0o755))
	require.NoError(t, os.WriteFile(archive, make([]byte, 80), 0o600))
	_, err = fmt.Fprintln(in, cache.ID)
	require.NoError(t, err)
	require.True(t, lines.Scan())
	require.Equal(t, "TRANSFERRING", lines.Text())
	// Model a download that lasts beyond the recent-use grace.
	db, err = h.openDB()
	require.NoError(t, err)
	require.NoError(t, db.Update(cache.ID, cache))
	require.NoError(t, db.Close())
	h.gcAt = time.Time{}
	h.gcCache()
	_, err = os.Stat(archive)
	require.NoError(t, err, "OS-wide transfer protection must prevent unlink")
	require.True(t, h.gcAt.IsZero(), "a blocked pass must remain immediately retryable")
	_, err = fmt.Fprintln(in, "RELEASE")
	require.NoError(t, err)
	// EOF proves the child finished and released its file lock.
	for lines.Scan() {
	}
	require.NoError(t, lines.Err())
	h.gcCache()
	db, err = h.openDB()
	require.NoError(t, err)
	defer db.Close()
	require.ErrorIs(t, db.Get(cache.ID, &Cache{}), bolthold.ErrNotFound)
	_, err = os.Stat(archive)
	require.ErrorIs(t, err, os.ErrNotExist, "expiration must resume after the transfer")
}
