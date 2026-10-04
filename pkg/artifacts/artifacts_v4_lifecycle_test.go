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
	route.rfs = trackedArtifactFS{file: tracked}
	route.AppURL = "localhost"
	route.prefix = ArtifactV4RouteBase
	req := httptest.NewRequest(http.MethodGet, route.buildArtifactURL("DownloadArtifact", "bundle", 1), nil)
	rec := httptest.NewRecorder()
	route.downloadArtifact(&ArtifactContext{Req: req, Resp: rec})
	require.Equal(t, "payload", rec.Body.String())
	require.True(t, tracked.closed, "a completed download must release its file before deletion can reclaim storage")
}
