package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type restoreArchiveContainer struct {
	container.Container
	archives []io.ReadCloser
}

func (action *restoreArchiveContainer) GetContainerArchive(_ context.Context, _ string) (io.ReadCloser, error) {
	archive := action.archives[0]
	action.archives = action.archives[1:]
	return archive, nil
}
func restoreTestArchive(t *testing.T, headers []*tar.Header, contents []string) io.ReadCloser {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for index, header := range headers {
		require.NoError(t, writer.WriteHeader(header))
		_, err := writer.Write([]byte(contents[index]))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return io.NopCloser(bytes.NewReader(data.Bytes()))
}
func TestHostDockerRestoreRejectsEscapingLinkBeforeMutation(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "original"), []byte("original"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "payload"), []byte("outside"), 0600))
	action := &restoreArchiveContainer{archives: []io.ReadCloser{restoreTestArchive(t, []*tar.Header{
		{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0777},
		{Name: "escape/payload", Typeflag: tar.TypeReg, Size: 7, Mode: 0600},
	}, []string{"", "changed"})}}
	transfer := &hostDockerTransfer{host: &container.HostEnvironment{}, staged: true, paths: []hostDockerPath{{host: root, action: "/action"}}}
	assert.Error(t, transfer.restore(action)(context.Background()))
	data, err := os.ReadFile(filepath.Join(outside, "payload"))
	require.NoError(t, err)
	assert.Equal(t, "outside", string(data))
	data, err = os.ReadFile(filepath.Join(root, "original"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
}
func TestHostDockerRestoreValidatesBothRootsBeforeMutation(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	paths := []hostDockerPath{}
	for _, root := range roots {
		require.NoError(t, os.WriteFile(filepath.Join(root, "payload"), []byte("original"), 0600))
		paths = append(paths, hostDockerPath{host: root, action: "/action"})
	}
	action := &restoreArchiveContainer{archives: []io.ReadCloser{
		restoreTestArchive(t, []*tar.Header{{Name: "payload", Typeflag: tar.TypeReg, Size: 7, Mode: 0600}}, []string{"changed"}),
		io.NopCloser(bytes.NewBufferString("malformed archive")),
	}}
	transfer := &hostDockerTransfer{host: &container.HostEnvironment{}, staged: true, paths: paths}
	require.Error(t, transfer.restore(action)(context.Background()))
	for _, root := range roots {
		data, err := os.ReadFile(filepath.Join(root, "payload"))
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
	}
}
func TestHostDockerRestorePreservesUnchangedAndEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "unchanged")
	require.NoError(t, os.WriteFile(file, []byte("same"), 0600))
	stamp := time.Unix(12345, 0)
	require.NoError(t, os.Chtimes(file, stamp, stamp))
	before, err := os.Stat(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "deleted"), []byte("delete"), 0600))
	action := &restoreArchiveContainer{archives: []io.ReadCloser{restoreTestArchive(t, []*tar.Header{
		{Name: "unchanged", Typeflag: tar.TypeReg, Size: 4, Mode: 0600, ModTime: stamp},
		{Name: "empty", Typeflag: tar.TypeDir, Mode: 0755},
	}, []string{"same", ""})}}
	transfer := &hostDockerTransfer{host: &container.HostEnvironment{}, staged: true, paths: []hostDockerPath{{host: root, action: "/action"}}}
	require.NoError(t, transfer.restore(action)(context.Background()))
	after, err := os.Stat(file)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	assert.Equal(t, before.ModTime(), after.ModTime())
	info, err := os.Stat(filepath.Join(root, "empty"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	_, err = os.Stat(filepath.Join(root, "deleted"))
	assert.True(t, os.IsNotExist(err))
}

func TestHostDockerRestoreRollsBackEarlierRootOnCommitError(t *testing.T) {
	paths := []hostDockerPath{{host: t.TempDir(), action: "/one"}, {host: t.TempDir(), action: "/two"}}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(filepath.Join(path.host, "payload"), []byte("original"), 0600))
	}
	action := &restoreArchiveContainer{archives: []io.ReadCloser{
		restoreTestArchive(t, []*tar.Header{{Name: "payload", Typeflag: tar.TypeReg, Size: 7, Mode: 0600}}, []string{"changed"}),
		restoreTestArchive(t, []*tar.Header{{Name: "payload", Typeflag: tar.TypeReg, Size: 7, Mode: 0600}}, []string{"changed"}),
	}}
	plans := []*hostRestorePlan{}
	for _, path := range paths {
		plan, err := prepareHostRestore(context.Background(), action, path)
		require.NoError(t, err)
		plans = append(plans, plan)
	}
	// Inject an I/O failure only after staging and validation completed. The first
	// root's writes must roll back when the second root cannot be reconciled.
	require.NoError(t, plans[1].root.Close())
	require.Error(t, applyHostRestores(context.Background(), plans))
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(path.host, "payload"))
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
	}
	require.NoError(t, plans[0].cleanup())
}
func TestHostDockerRestorePreservesInternalLinkAndDirectoryMode(t *testing.T) {
	root := t.TempDir()
	action := &restoreArchiveContainer{archives: []io.ReadCloser{restoreTestArchive(t, []*tar.Header{
		{Name: "directory", Typeflag: tar.TypeDir, Mode: 0750},
		{Name: "directory/payload", Typeflag: tar.TypeReg, Mode: 0600, Size: 4},
		{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "directory/payload"},
	}, []string{"", "data", ""})}}
	transfer := &hostDockerTransfer{host: &container.HostEnvironment{}, staged: true, paths: []hostDockerPath{{host: root, action: "/action"}}}
	require.NoError(t, transfer.restore(action)(context.Background()))
	data, err := os.ReadFile(filepath.Join(root, "link"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(data))
	info, err := os.Stat(filepath.Join(root, "directory"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0750), info.Mode().Perm())
}
func TestHostDockerRestoreRejectsOversizedStreamBeforeMutation(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0600, Size: hostRestoreBytes + 1}))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "original"), []byte("original"), 0600))
	action := &restoreArchiveContainer{archives: []io.ReadCloser{io.NopCloser(bytes.NewReader(data.Bytes()))}}
	transfer := &hostDockerTransfer{host: &container.HostEnvironment{}, staged: true, paths: []hostDockerPath{{host: root, action: "/action"}}}
	require.ErrorContains(t, transfer.restore(action)(context.Background()), "byte limit")
	contents, err := os.ReadFile(filepath.Join(root, "original"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(contents))
}
