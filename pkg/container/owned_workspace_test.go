package container

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreOwnedTree(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "hostexecutor")
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "deleted"), []byte("old"), 0o600))
	host := HostEnvironment{OwnedRoot: root, Path: workspace}
	archive := new(bytes.Buffer)
	modified := time.Unix(1720000000, 123456789)
	writer := tar.NewWriter(archive)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: modified, Format: tar.FormatPAX}))
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./script", Typeflag: tar.TypeReg, Mode: 0o751, Size: 3, ModTime: modified, Format: tar.FormatPAX}))
	_, err := writer.Write([]byte("new"))
	require.NoError(t, err)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "script", Mode: 0o777}))
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "./hardlink", Typeflag: tar.TypeLink, Linkname: "./script", Mode: 0o751}))
	require.NoError(t, writer.Close())
	require.NoError(t, host.RestoreOwnedTree(context.Background(), workspace, archive))
	content, err := os.ReadFile(filepath.Join(workspace, "script"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(content))
	info, err := os.Stat(filepath.Join(workspace, "script"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o751), info.Mode().Perm())
	assert.Equal(t, modified.UnixNano(), info.ModTime().UnixNano())
	directoryInfo, err := os.Stat(workspace)
	require.NoError(t, err)
	assert.Equal(t, modified.UnixNano(), directoryInfo.ModTime().UnixNano())
	hardlinkInfo, err := os.Stat(filepath.Join(workspace, "hardlink"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(info, hardlinkInfo))
	target, err := os.Readlink(filepath.Join(workspace, "link"))
	require.NoError(t, err)
	assert.Equal(t, "script", target)
	_, err = os.Stat(filepath.Join(workspace, "deleted"))
	assert.True(t, os.IsNotExist(err))
}

func TestRestoreOwnedTreeRejectsUnsafeArchiveWithoutChangingWorkspace(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/escape", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/escape"},
		{Name: "device", Typeflag: tar.TypeChar},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "../../escape"},
	} {
		t.Run(header.Name+header.Linkname, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "hostexecutor")
			require.NoError(t, os.Mkdir(workspace, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "retained"), []byte("old"), 0o600))
			host := HostEnvironment{OwnedRoot: root, Path: workspace}
			archive := new(bytes.Buffer)
			writer := tar.NewWriter(archive)
			require.NoError(t, writer.WriteHeader(header))
			require.NoError(t, writer.Close())
			assert.Error(t, host.RestoreOwnedTree(context.Background(), workspace, archive))
			content, err := os.ReadFile(filepath.Join(workspace, "retained"))
			require.NoError(t, err)
			assert.Equal(t, "old", string(content))
		})
	}
}

func TestRestoreOwnedTreeRejectsUnownedDestination(t *testing.T) {
	root := t.TempDir()
	host := HostEnvironment{Path: root}
	assert.Error(t, host.RestoreOwnedTree(context.Background(), root, bytes.NewReader(nil)))
}
