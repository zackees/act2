package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnedArchiveRoundTripPreservesExactMetadata(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "hostexecutor")
	require.NoError(t, os.Mkdir(workspace, 0o755))
	file := filepath.Join(workspace, "script")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	require.NoError(t, os.Chmod(file, 0o751))
	require.NoError(t, os.Symlink("script", filepath.Join(workspace, "link")))
	modified := time.Unix(1720000000, 123456789)
	require.NoError(t, os.Chtimes(file, modified, modified))
	require.NoError(t, os.Chtimes(workspace, modified, modified))
	beforeLink, err := os.Lstat(filepath.Join(workspace, "link"))
	require.NoError(t, err)
	host := HostEnvironment{OwnedRoot: root, Path: workspace}
	archive, err := host.GetOwnedTreeArchive(context.Background(), workspace)
	require.NoError(t, err)
	defer archive.Close()
	require.NoError(t, host.RestoreOwnedTree(context.Background(), workspace, archive))
	for _, name := range []string{file, workspace} {
		info, err := os.Stat(name)
		require.NoError(t, err)
		assert.Equal(t, modified.UnixNano(), info.ModTime().UnixNano())
	}
	afterLink, err := os.Lstat(filepath.Join(workspace, "link"))
	require.NoError(t, err)
	assert.Equal(t, beforeLink.ModTime(), afterLink.ModTime())
	assert.True(t, os.SameFile(beforeLink, afterLink))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o751), info.Mode().Perm())
}

func TestOwnedArchiveCancellationReleasesStreamingProducer(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "hostexecutor")
	require.NoError(t, os.Mkdir(workspace, 0o755))
	host := HostEnvironment{OwnedRoot: root, Path: workspace}
	ctx, cancel := context.WithCancel(context.Background())
	archive, err := host.GetOwnedTreeArchive(ctx, workspace)
	require.NoError(t, err)
	defer archive.Close()
	cancel()
	_, err = io.Copy(io.Discard, archive)
	assert.True(t, errors.Is(err, context.Canceled))
}

type oversizedOwnedFileInfo struct{ fs.FileInfo }

func (oversizedOwnedFileInfo) Size() int64 { return ownedArchiveByteLimit + 1 }

func TestOwnedArchiveRejectsExpandedLimitBeforeReadingPayload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "small")
	require.NoError(t, os.WriteFile(file, []byte("small"), 0o600))
	info, err := os.Stat(file)
	require.NoError(t, err)
	output := new(bytes.Buffer)
	collector := ownedTarCollector{writer: tar.NewWriter(output)}
	err = collector.WriteFile("large", oversizedOwnedFileInfo{info}, "", nil)
	assert.ErrorContains(t, err, "exceeds workspace limits")
	assert.Zero(t, output.Len())
}

func TestOwnedArchiveWriterEnforcesWireLimit(t *testing.T) {
	output := new(bytes.Buffer)
	writer := ownedArchiveWriter{ctx: context.Background(), writer: output, remaining: 1}
	n, err := writer.Write([]byte("too large"))
	assert.Zero(t, n)
	assert.ErrorContains(t, err, "exceeds workspace limits")
	assert.Zero(t, output.Len())
}
