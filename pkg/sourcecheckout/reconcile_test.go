package sourcecheckout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcilePreservesIdenticalSource(t *testing.T) {
	destination, staging := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0600))
	}
	for _, root := range []string{destination, staging} {
		write(root, "src/same.go", "same")
		write(root, "src/change.go", "old")
	}
	write(destination, "gone.go", "delete")
	write(destination, "target/keep", "output")
	write(destination, ".git/config", "trusted")
	previous, err := BuildEnvelope(destination, []string{"src/same.go", "src/change.go", "gone.go"}, DefaultLimits())
	require.NoError(t, err)
	write(staging, "src/change.go", "new")
	write(staging, "added.go", "added")
	next, err := BuildEnvelope(staging, []string{"src/same.go", "src/change.go", "added.go"}, DefaultLimits())
	require.NoError(t, err)
	precise := time.Unix(12345, 987654321)
	require.NoError(t, os.Chtimes(filepath.Join(destination, "src/same.go"), precise, precise))
	before, err := os.Stat(filepath.Join(destination, "src/same.go"))
	require.NoError(t, err)
	result, err := Reconcile(destination, staging, previous, next, DefaultLimits())
	require.NoError(t, err)
	after, err := os.Stat(filepath.Join(destination, "src/same.go"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	assert.Equal(t, before.ModTime(), after.ModTime())
	assert.Equal(t, 1, result.Unchanged)
	contents, err := os.ReadFile(filepath.Join(destination, "src/change.go"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(contents))
	_, err = os.Stat(filepath.Join(destination, "gone.go"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{"target/keep", ".git/config"} {
		_, err = os.Stat(filepath.Join(destination, name))
		require.NoError(t, err)
	}
}

func TestReconcileSourceTransitions(t *testing.T) {
	destination, staging := t.TempDir(), t.TempDir()
	write := func(root, name, body string, mode os.FileMode) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), mode))
	}
	write(destination, "file-to-dir", "old", 0600)
	write(destination, "dir-to-file/child", "old", 0600)
	write(destination, "mode", "same", 0600)
	write(destination, "rename-old", "same", 0600)
	write(destination, "link-to-file", "old", 0600)
	require.NoError(t, os.Symlink("mode", filepath.Join(destination, "file-to-link")))
	previous, err := BuildEnvelope(destination, []string{"file-to-dir", "dir-to-file/child", "mode", "rename-old", "link-to-file", "file-to-link"}, DefaultLimits())
	require.NoError(t, err)
	write(staging, "file-to-dir/child", "new", 0600)
	write(staging, "dir-to-file", "new", 0600)
	write(staging, "mode", "same", 0755)
	write(staging, "rename-new", "same", 0600)
	write(staging, "file-to-link", "new", 0600)
	require.NoError(t, os.Symlink("mode", filepath.Join(staging, "link-to-file")))
	next, err := BuildEnvelope(staging, []string{"file-to-dir/child", "dir-to-file", "mode", "rename-new", "link-to-file", "file-to-link"}, DefaultLimits())
	require.NoError(t, err)
	_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(destination, "mode"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0111)
	link, err := os.Readlink(filepath.Join(destination, "link-to-file"))
	require.NoError(t, err)
	assert.Equal(t, "mode", link)
	for _, name := range []string{"file-to-dir/child", "dir-to-file", "rename-new", "file-to-link"} {
		contents, err := os.ReadFile(filepath.Join(destination, name))
		require.NoError(t, err)
		assert.NotEmpty(t, contents)
	}
	_, err = os.Lstat(filepath.Join(destination, "rename-old"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReconcileRejectsBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"tampered-stage", "unlisted-directory", "symlink-parent", "tampered-envelope", "oversized", "unlisted-empty-directory", "late-stage-tamper"} {
		t.Run(scenario, func(t *testing.T) {
			destination, staging := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(destination, "a"), []byte("original"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(staging, "a"), []byte("changed"), 0600))
			previous, err := BuildEnvelope(destination, []string{"a"}, DefaultLimits())
			require.NoError(t, err)
			next, err := BuildEnvelope(staging, []string{"a"}, DefaultLimits())
			require.NoError(t, err)
			limits := DefaultLimits()
			switch scenario {
			case "late-stage-tamper":
				require.NoError(t, os.WriteFile(filepath.Join(staging, "z"), []byte("valid"), 0600))
				next, err = BuildEnvelope(staging, []string{"a", "z"}, DefaultLimits())
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(staging, "z"), []byte("forged"), 0600))
			case "unlisted-empty-directory":
				require.NoError(t, os.MkdirAll(filepath.Join(destination, "collision/.git"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "collision"), []byte("new"), 0600))
				next, err = BuildEnvelope(staging, []string{"a", "collision"}, DefaultLimits())
				require.NoError(t, err)
			case "tampered-stage":
				require.NoError(t, os.WriteFile(filepath.Join(staging, "a"), []byte("forged"), 0600))
			case "tampered-envelope":
				next.Entries[0].SHA256 = "bad"
			case "oversized":
				limits.TotalBytes = 1
			case "unlisted-directory":
				require.NoError(t, os.Mkdir(filepath.Join(destination, "collision"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(destination, "collision/keep"), []byte("output"), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "collision"), []byte("new"), 0600))
				next, err = BuildEnvelope(staging, []string{"a", "collision"}, DefaultLimits())
				require.NoError(t, err)
			case "symlink-parent":
				require.NoError(t, os.Mkdir(filepath.Join(staging, "nested"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "nested/source"), []byte("new"), 0600))
				next, err = BuildEnvelope(staging, []string{"a", "nested/source"}, DefaultLimits())
				require.NoError(t, err)
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(destination, "nested")))
			}
			_, err = Reconcile(destination, staging, previous, next, limits)
			require.Error(t, err)
			contents, err := os.ReadFile(filepath.Join(destination, "a"))
			require.NoError(t, err)
			assert.Equal(t, "original", string(contents))
		})
	}
}

func TestEnvelopeRejectsProtectedAndEscapingPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escape", ".git/config", "nested/.git/config", "target/output", "/absolute", "back\\slash"} {
		_, err := BuildEnvelope(root, []string{name}, DefaultLimits())
		require.Error(t, err)
	}
	require.NoError(t, os.Symlink("../outside", filepath.Join(root, "escape")))
	_, err := BuildEnvelope(root, []string{"escape"}, DefaultLimits())
	require.Error(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "source"), []byte("1234"), 0600))
	limits := DefaultLimits()
	limits.TotalBytes = 3
	_, err = BuildEnvelope(root, []string{"source"}, limits)
	require.Error(t, err)
}

func TestReconcileDoesNotWriteThroughHardlinks(t *testing.T) {
	destination, staging := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(destination, "target"), 0755))
	protected := filepath.Join(destination, "target/output")
	require.NoError(t, os.WriteFile(protected, []byte("compiled"), 0600))
	require.NoError(t, os.Link(protected, filepath.Join(destination, "source")))
	previous, err := BuildEnvelope(destination, []string{"source"}, DefaultLimits())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "source"), []byte("changed"), 0600))
	next, err := BuildEnvelope(staging, []string{"source"}, DefaultLimits())
	require.NoError(t, err)
	_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
	require.NoError(t, err)
	contents, err := os.ReadFile(protected)
	require.NoError(t, err)
	assert.Equal(t, "compiled", string(contents))
}

func TestEnvelopeBindingAndInventoryBounds(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source"), []byte("one"), 0600))
	envelope, err := BuildEnvelope(root, []string{"source"}, DefaultLimits())
	require.NoError(t, err)
	_, err = envelope.Bind(Binding{SourceCommit: "short", GitTree: "short"})
	require.Error(t, err)
	bound, err := envelope.Bind(Binding{SourceCommit: "0123456789012345678901234567890123456789", GitTree: "abcdefabcdefabcdefabcdefabcdefabcdefabcd", OutputIdentity: "compiler-target-config"})
	require.NoError(t, err)
	assert.NotEqual(t, envelope.ContentID, bound.ContentID)
	_, err = BuildEnvelope(root, []string{"source", "source"}, DefaultLimits())
	require.Error(t, err)
	paths := make([]string, 10001)
	_, err = BuildEnvelope(root, paths, DefaultLimits())
	require.Error(t, err)
	require.NoError(t, os.Symlink("target/output", filepath.Join(root, "link")))
	_, err = BuildEnvelope(root, []string{"link"}, DefaultLimits())
	require.Error(t, err)
}
