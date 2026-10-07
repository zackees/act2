package artifacts

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV4EmptyBlockListPreservesCommittedArchive(t *testing.T) {
	route := boundedArtifactRoute(t)
	route.limits.MaxTotalBytes = 32
	require.Equal(t, http.StatusCreated, stageTestBlock(t, route, "bundle", "one", "ORIGINAL"))
	commit := func(body string) int {
		response := httptest.NewRecorder()
		route.commitArtifactBlocks(&ArtifactContext{Req: httptest.NewRequest(http.MethodPut, "http://localhost/", strings.NewReader(body)), Resp: response}, 1, "bundle")
		return response.Code
	}
	require.Equal(t, http.StatusCreated, commit(`<BlockList><Latest>one</Latest></BlockList>`))
	status := commit(`<BlockList/>`)
	data, err := os.ReadFile(filepath.Join(route.baseDir, "1", "bundle", "bundle.zip"))
	require.NoError(t, err)
	require.Equal(t, "ORIGINAL", string(data), "an empty commit must retain the existing archive")
	require.Equal(t, http.StatusBadRequest, status)
}

type failingAppendFS struct {
	WriteFS
	file WritableFile
	err  error
}

func (fsys failingAppendFS) OpenAppendable(string) (WritableFile, error) { return fsys.file, fsys.err }

type failingAppendFile struct {
	writeErr error
	closeErr error
	closes   int
}

func (file *failingAppendFile) Write(data []byte) (int, error) {
	if file.writeErr != nil {
		return 0, file.writeErr
	}
	return len(data), nil
}
func (file *failingAppendFile) Close() error { file.closes++; return file.closeErr }

func TestV4AppendReportsStorageErrors(t *testing.T) {
	failure := errors.New("owned append storage failure")
	cases := []struct {
		name     string
		openErr  error
		writeErr error
		closeErr error
	}{
		{name: "open", openErr: failure}, {name: "write", writeErr: failure}, {name: "close", closeErr: failure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := boundedArtifactRoute(t)
			route.AppURL = "localhost"
			route.prefix = ArtifactV4RouteBase
			file := &failingAppendFile{writeErr: tc.writeErr, closeErr: tc.closeErr}
			route.fs = failingAppendFS{file: file, err: tc.openErr}
			response := httptest.NewRecorder()
			target := route.buildArtifactURL("UploadArtifact", "bundle", 1) + "&comp=appendBlock"
			require.NotPanics(t, func() {
				route.uploadArtifact(&ArtifactContext{Req: httptest.NewRequest(http.MethodPut, target, strings.NewReader("payload")), Resp: response})
			})
			require.Equal(t, http.StatusInternalServerError, response.Code)
			if tc.openErr == nil {
				require.Equal(t, 1, file.closes, "each opened file must close once, including failures")
			}
		})
	}
}
