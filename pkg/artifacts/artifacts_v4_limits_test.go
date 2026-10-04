package artifacts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boundedArtifactRoute(t *testing.T) *artifactV4Routes {
	t.Helper()
	return &artifactV4Routes{baseDir: t.TempDir(), blockState: &artifactBlockState{}, fs: readWriteFSImpl{}, rfs: readWriteFSImpl{}, limits: ArtifactLimits{MaxBlockBytes: 8, MaxBlocks: 2, MaxTotalBytes: 10}}
}

func stageTestBlock(t *testing.T, route *artifactV4Routes, artifact, id, data string) int {
	t.Helper()
	// A chunked body has no Content-Length: bounding headers alone is insufficient.
	req := httptest.NewRequest(http.MethodPut, "http://localhost/", io.NopCloser(strings.NewReader(data)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	route.stageArtifactBlock(&ArtifactContext{Req: req, Resp: rec}, 1, artifact, id)
	return rec.Code
}

func TestV4BlockStagingRejectsSymlinkParent(t *testing.T) {
	route := boundedArtifactRoute(t)
	outside := t.TempDir()
	artifact := filepath.Join(route.baseDir, "1", "bundle")
	require.NoError(t, os.MkdirAll(artifact, 0755))
	require.NoError(t, os.Symlink(outside, filepath.Join(artifact, ".blocks")))
	assert.GreaterOrEqual(t, stageTestBlock(t, route, "bundle", "one", "data"), 400)
	files, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, files, "a staging-parent symlink must never write outside the artifact tree")
}

func TestV4BlockStagingBoundsStreamAndBudget(t *testing.T) {
	t.Run("unknown-length stream", func(t *testing.T) {
		route := boundedArtifactRoute(t)
		assert.Equal(t, http.StatusRequestEntityTooLarge, stageTestBlock(t, route, "bundle", "one", "123456789"))
	})
	t.Run("block count", func(t *testing.T) {
		route := boundedArtifactRoute(t)
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "1"))
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "two", "2"))
		assert.Equal(t, http.StatusRequestEntityTooLarge, stageTestBlock(t, route, "bundle", "three", "3"))
	})
	t.Run("aggregate across artifacts", func(t *testing.T) {
		route := boundedArtifactRoute(t)
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "first", "one", "123456"))
		assert.Equal(t, http.StatusRequestEntityTooLarge, stageTestBlock(t, route, "second", "one", "12345"))
	})
	t.Run("retry replaces its budget", func(t *testing.T) {
		route := boundedArtifactRoute(t)
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "123456"))
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "12"))
		require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "two", "12345678"))
	})
}

func TestV4BlockBudgetIncludesExistingFiles(t *testing.T) {
	route := boundedArtifactRoute(t)
	first := route.artifactBlockPath(1, "first", "one")
	require.NoError(t, os.MkdirAll(filepath.Dir(first), 0755))
	require.NoError(t, os.WriteFile(first, []byte("123456"), 0600))
	assert.Equal(t, http.StatusRequestEntityTooLarge, stageTestBlock(t, route, "second", "one", "12345"))
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "first", "one", "12"))
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "second", "one", "12345678"))
	assertArtifactDiskBudget(t, route)
}

func TestV4BlockBudgetIsSharedByConcurrentUploads(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxBlocks = 10
	statuses := make(chan int, 3)
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two", "three"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			statuses <- stageTestBlock(t, route, "bundle", id, "123456")
		}(id)
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusCreated {
			successes++
		} else {
			assert.Equal(t, http.StatusRequestEntityTooLarge, status)
		}
	}
	assert.Equal(t, 1, successes)
	assertArtifactDiskBudget(t, route)
}

func assertArtifactDiskBudget(t *testing.T, route *artifactV4Routes) {
	t.Helper()
	var total int64
	err := filepath.WalkDir(route.baseDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, total, route.limits.MaxTotalBytes, "actual disk usage must stay within the shared budget")
}

func TestV4BlockBudgetIncludesAssembledArchive(t *testing.T) {
	route := boundedArtifactRoute(t)
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "123456"))
	commit := func() int {
		req := httptest.NewRequest(http.MethodPut, "http://localhost/", strings.NewReader(`<BlockList><Latest>one</Latest></BlockList>`))
		rec := httptest.NewRecorder()
		route.commitArtifactBlocks(&ArtifactContext{Req: req, Resp: rec}, 1, "bundle")
		return rec.Code
	}
	assert.Equal(t, http.StatusRequestEntityTooLarge, commit())
	assertArtifactDiskBudget(t, route)
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "12"))
	require.Equal(t, http.StatusCreated, commit())
	assertArtifactDiskBudget(t, route)
}
