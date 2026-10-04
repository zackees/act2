package artifacts

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
)

type pausedArtifactReader struct {
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
	data    io.Reader
}

func (reader *pausedArtifactReader) Read(data []byte) (int, error) {
	reader.once.Do(func() { close(reader.ready) })
	<-reader.release
	return reader.data.Read(data)
}

func TestV4BlockCommitWaitsForRetryCompletion(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxTotalBytes = 32
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "OLD"))
	ready := make(chan struct{})
	release := make(chan struct{})
	retryDone := make(chan int, 1)
	reader := io.MultiReader(strings.NewReader("NE"), &pausedArtifactReader{ready: ready, release: release, data: strings.NewReader("WBLOCK")})
	go func() {
		req := httptest.NewRequest(http.MethodPut, "http://localhost/", io.NopCloser(reader))
		req.ContentLength = -1
		rec := httptest.NewRecorder()
		route.stageArtifactBlock(&ArtifactContext{Req: req, Resp: rec}, 1, "bundle", "one")
		retryDone <- rec.Code
	}()
	<-ready // The retry has truncated the old block and written only its prefix.
	commitDone := make(chan struct{})
	go func() {
		// Release the paused producer even when a correct commit waits for its lock.
		// This is test coordination, not a runtime or performance assertion.
		select {
		case <-commitDone:
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
	}()
	req := httptest.NewRequest(http.MethodPut, "http://localhost/", strings.NewReader(`<BlockList><Latest>one</Latest></BlockList>`))
	rec := httptest.NewRecorder()
	route.commitArtifactBlocks(&ArtifactContext{Req: req, Resp: rec}, 1, "bundle")
	close(commitDone)
	require.Equal(t, http.StatusCreated, <-retryDone)
	require.Equal(t, http.StatusCreated, rec.Code)
	data, err := os.ReadFile(filepath.Join(route.baseDir, "1", "bundle", "bundle.zip"))
	require.NoError(t, err)
	require.Equal(t, "NEWBLOCK", string(data), "a successful commit must snapshot the completed retry, not its partial prefix")
}

func TestV4BlockDeleteReclaimsBudgetAndCount(t *testing.T) {
	route := boundedArtifactRoute(t)
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "123456"))
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(`{"workflow_run_backend_id":"1","workflow_job_run_backend_id":"1","name":"bundle"}`))
	rec := httptest.NewRecorder()
	route.deleteArtifact(&ArtifactContext{Req: req, Resp: rec})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "new-one", "12345678"))
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "new-two", "12"))
	assertArtifactDiskBudget(t, route)
}

type trackedArtifactFile struct {
	fs.File
	closed bool
}

func (file *trackedArtifactFile) Close() error { file.closed = true; return file.File.Close() }

type trackedArtifactFS struct{ file *trackedArtifactFile }

func (fsys trackedArtifactFS) Open(_ string) (fs.File, error) { return fsys.file, nil }

func TestV4BlockDownloadClosesFileBeforeDeletion(t *testing.T) {
	file, err := fstest.MapFS{"payload.zip": {Data: []byte("payload")}}.Open("payload.zip")
	require.NoError(t, err)
	tracked := &trackedArtifactFile{File: file}
	route := boundedArtifactRoute(t)
	archive := filepath.Join(route.baseDir, "1", "bundle", "bundle.zip")
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0755))
	require.NoError(t, os.WriteFile(archive, []byte("payload"), 0600))
	route.rfs = trackedArtifactFS{file: tracked}
	route.AppURL = "localhost"
	route.prefix = ArtifactV4RouteBase
	req := httptest.NewRequest(http.MethodGet, route.buildArtifactURL("DownloadArtifact", "bundle", 1), nil)
	rec := httptest.NewRecorder()
	route.downloadArtifact(&ArtifactContext{Req: req, Resp: rec})
	require.Equal(t, "payload", rec.Body.String())
	require.True(t, tracked.closed, "a completed download must release its file before deletion can reclaim storage")
}

// Open runs the first commit exactly after download's archive lookup. A missing
// entry must reject the request before filesystem access can race that commit.
type firstCommitArtifactFS struct {
	open func(string) (fs.File, error)
}

func (fsys firstCommitArtifactFS) Open(name string) (fs.File, error) { return fsys.open(name) }

func TestV4BlockDownloadRejectsMissingEntryBeforeFirstCommit(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxTotalBytes = 32
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "payload"))
	route.AppURL = "localhost"
	route.prefix = ArtifactV4RouteBase
	opened := false
	route.rfs = firstCommitArtifactFS{open: func(name string) (fs.File, error) {
		opened = true
		req := httptest.NewRequest(http.MethodPut, "http://localhost/", strings.NewReader(`<BlockList><Latest>one</Latest></BlockList>`))
		rec := httptest.NewRecorder()
		route.commitArtifactBlocks(&ArtifactContext{Req: req, Resp: rec}, 1, "bundle")
		require.Equal(t, http.StatusCreated, rec.Code)
		return os.Open(name)
	}}
	req := httptest.NewRequest(http.MethodGet, route.buildArtifactURL("DownloadArtifact", "bundle", 1), nil)
	rec := httptest.NewRecorder()
	route.downloadArtifact(&ArtifactContext{Req: req, Resp: rec})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.False(t, opened, "a lookup miss must not open an archive created by a racing first commit")
}

func TestV4BlockInitialDownloadLocksPreexistingArchive(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxTotalBytes = 32
	archive := filepath.Join(route.baseDir, "1", "bundle", "bundle.zip")
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0755))
	require.NoError(t, os.WriteFile(archive, []byte("original"), 0600))
	route.AppURL = "localhost"
	route.prefix = ArtifactV4RouteBase
	route.rfs = firstCommitArtifactFS{open: func(name string) (fs.File, error) {
		// The first staging request may initialize the inventory after download has
		// selected its path. Attempt the same exclusive lease used by assembly.
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "new"))
		store := route.blockState.store.Load()
		entry := store.lookup("1/bundle/bundle.zip")
		require.NotNil(t, entry)
		if entry.mu.TryLock() {
			// Reproduce the partial prefix visible while a first commit is streaming.
			require.NoError(t, os.WriteFile(archive, []byte("partial"), 0600))
			entry.mu.Unlock()
		}
		return os.Open(name)
	}}
	req := httptest.NewRequest(http.MethodGet, route.buildArtifactURL("DownloadArtifact", "bundle", 1), nil)
	rec := httptest.NewRecorder()
	route.downloadArtifact(&ArtifactContext{Req: req, Resp: rec})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "original", rec.Body.String(), "initial inventory must lease the preexisting archive before opening it")
}

func TestV4FinalizeWaitsForCompletedArchive(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxTotalBytes = 32
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "OLD"))
	commit := httptest.NewRecorder()
	route.commitArtifactBlocks(&ArtifactContext{Req: httptest.NewRequest(http.MethodPut, "http://localhost/", strings.NewReader(`<BlockList><Latest>one</Latest></BlockList>`)), Resp: commit}, 1, "bundle")
	require.Equal(t, http.StatusCreated, commit.Code)
	archive := filepath.Join(route.baseDir, "1", "bundle", "bundle.zip")
	entry := route.blockState.store.Load().lookup(filepath.Join("1", "bundle", "bundle.zip"))
	require.NotNil(t, entry)
	entry.mu.Lock()
	require.NoError(t, os.WriteFile(archive, []byte("N"), 0600))
	done := make(chan struct{})
	rewritten := make(chan error, 1)
	go func() {
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		rewritten <- os.WriteFile(archive, []byte("NEWPAYLOAD"), 0600)
		entry.mu.Unlock()
	}()
	response := httptest.NewRecorder()
	route.finalizeArtifact(&ArtifactContext{Req: httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(`{"workflow_run_backend_id":"1","workflow_job_run_backend_id":"1","name":"bundle","size":"10"}`)), Resp: response})
	close(done)
	require.NoError(t, <-rewritten)
	require.Equal(t, http.StatusOK, response.Code, "finalization must validate a completed archive while holding its read lock")
}
