package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nektos/act/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDockerActionReconcilesBeforeReturningFailureAndCancellation(t *testing.T) {
	root := t.TempDir()
	host := &container.HostEnvironment{OwnedRoot: root, Path: filepath.Join(root, "hostexecutor"), ActPath: filepath.Join(root, "act")}
	for _, directory := range []string{host.Path, host.ActPath} {
		require.NoError(t, os.Mkdir(directory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "deleted"), []byte("old"), 0o600))
	}
	rc := RunContext{JobContainer: host}
	action := &containerMock{}
	for _, directory := range []string{host.Path, host.ActPath} {
		action.On("CopyTarStream", mock.Anything, directory, mock.Anything).Return(nil).Once()
		archive := new(bytes.Buffer)
		writer := tar.NewWriter(archive)
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}))
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./output", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6}))
		_, err := writer.Write([]byte("copied"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		action.On("GetContainerArchive", mock.MatchedBy(func(ctx context.Context) bool { return ctx.Err() == nil }), directory+"/.").Return(io.NopCloser(archive), nil).Once()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("action failed")
	action.On("Start", true).Return(func(context.Context) error { cancel(); return failure }).Once()
	assert.ErrorIs(t, rc.startDockerAction(action)(ctx), failure)
	for _, directory := range []string{host.Path, host.ActPath} {
		content, err := os.ReadFile(filepath.Join(directory, "output"))
		require.NoError(t, err)
		assert.Equal(t, "copied", string(content))
		_, err = os.Stat(filepath.Join(directory, "deleted"))
		assert.True(t, os.IsNotExist(err))
	}
	action.AssertExpectations(t)
}
