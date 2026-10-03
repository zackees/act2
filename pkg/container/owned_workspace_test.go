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

func TestRestoreOwnedTreeCoarseTimestamps(t *testing.T) {
	for _, scenario := range []string{"unchanged", "content", "mode", "entries", "time"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "hostexecutor")
			require.NoError(t, os.Mkdir(workspace, 0o755))
			original := time.Unix(1720000000, 123456789)
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "file"), []byte("old"), 0o600))
			require.NoError(t, os.Chmod(filepath.Join(workspace, "file"), 0o751))
			require.NoError(t, os.Chtimes(filepath.Join(workspace, "file"), original, original))
			require.NoError(t, os.Chtimes(workspace, original, original))
			content, mode, coarse := "old", int64(0o751), time.Unix(original.Unix(), 0)
			if scenario == "content" {
				content = "new"
			}
			if scenario == "mode" {
				mode = 0o750
			}
			if scenario == "time" {
				coarse = coarse.Add(time.Second)
			}
			archive := new(bytes.Buffer)
			writer := tar.NewWriter(archive)
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: coarse}))
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: mode, Size: 3, ModTime: coarse}))
			_, err := writer.Write([]byte(content))
			require.NoError(t, err)
			if scenario == "entries" {
				require.NoError(t, writer.WriteHeader(&tar.Header{Name: "new", Typeflag: tar.TypeReg, Mode: 0o600, ModTime: coarse}))
			}
			require.NoError(t, writer.Close())
			host := HostEnvironment{OwnedRoot: root, Path: workspace}
			require.NoError(t, host.RestoreOwnedTree(context.Background(), workspace, archive))
			file, err := os.Stat(filepath.Join(workspace, "file"))
			require.NoError(t, err)
			expectedFile := coarse
			if scenario == "unchanged" || scenario == "entries" {
				expectedFile = original
			}
			assert.Equal(t, expectedFile.UnixNano(), file.ModTime().UnixNano())
			directory, err := os.Stat(workspace)
			require.NoError(t, err)
			expectedDirectory := original
			if scenario == "entries" || scenario == "time" {
				expectedDirectory = coarse
			}
			assert.Equal(t, expectedDirectory.UnixNano(), directory.ModTime().UnixNano())
		})
	}
}
