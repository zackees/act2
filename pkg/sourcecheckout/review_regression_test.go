package sourcecheckout

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An untrusted manifest can reseal internally consistent but unsafe metadata.
func unsafeFixtureEnvelope(t *testing.T, root string, names []string) Envelope {
	t.Helper()
	result := Envelope{Version: 1}
	for _, name := range names {
		filename := filepath.Join(root, name)
		info, err := os.Lstat(filename)
		require.NoError(t, err)
		entry := Entry{Path: name, Size: info.Size(), Executable: info.Mode()&0111 != 0}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.Kind = "symlink"
			entry.Executable = false
			entry.Link, err = os.Readlink(filename)
			require.NoError(t, err)
		} else {
			entry.Kind = "file"
			data, err := os.ReadFile(filename)
			require.NoError(t, err)
			sum := sha256.Sum256(data)
			entry.SHA256 = hex.EncodeToString(sum[:])
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	result.ContentID = envelopeID(result)
	return result
}

func TestRejectCompleteMixedCaseAncestorInventory(t *testing.T) {
	for _, names := range [][]string{{"A/x", "a"}, {"a", "A/x"}, {"Dir/A/x", "dir/a"}, {"dir/a", "Dir/A/x"}, {"A/x", "a/y"}, {"Σ/x", "ς"}} {
		t.Run(names[0]+"-"+names[1], func(t *testing.T) {
			destination, staging := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(destination, "seed"), []byte("original"), 0600))
			previous, err := BuildEnvelope(destination, []string{"seed"}, DefaultLimits())
			require.NoError(t, err)
			for _, name := range names {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(staging, name)), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte("new"), 0600))
			}
			_, err = BuildEnvelope(staging, names, DefaultLimits())
			assert.Error(t, err)
			next := unsafeFixtureEnvelope(t, staging, names)
			_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
			assert.Error(t, err)
			contents, err := os.ReadFile(filepath.Join(destination, "seed"))
			require.NoError(t, err)
			assert.Equal(t, "original", string(contents))
		})
	}
}

func TestRejectUnlistedSymlinkAliasBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"destination-outside", "destination-protected", "staging-outside", "alias-chain", "cycle", "directory-child-alias"} {
		t.Run(scenario, func(t *testing.T) {
			destination, staging, outside := t.TempDir(), t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(destination, "seed"), []byte("original"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(staging, "seed"), []byte("changed"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "output"), []byte("protected"), 0600))
			previous, err := BuildEnvelope(destination, []string{"seed"}, DefaultLimits())
			require.NoError(t, err)
			linkTarget := "alias/output"
			names := []string{"seed", "link"}
			if scenario == "directory-child-alias" {
				linkTarget = "src"
				names = append(names, "src/source")
				require.NoError(t, os.Mkdir(filepath.Join(staging, "src"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "src/source"), []byte("source"), 0600))
			}
			require.NoError(t, os.Symlink(linkTarget, filepath.Join(staging, "link")))
			switch scenario {
			case "destination-outside":
				require.NoError(t, os.Symlink(outside, filepath.Join(destination, "alias")))
			case "destination-protected":
				require.NoError(t, os.Mkdir(filepath.Join(destination, "target"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(destination, "target/output"), []byte("compiled"), 0600))
				require.NoError(t, os.Symlink("target", filepath.Join(destination, "alias")))
			case "staging-outside":
				require.NoError(t, os.Symlink(outside, filepath.Join(staging, "alias")))
			case "alias-chain":
				require.NoError(t, os.Symlink("second", filepath.Join(destination, "alias")))
				require.NoError(t, os.Symlink(outside, filepath.Join(destination, "second")))
			case "directory-child-alias":
				require.NoError(t, os.Mkdir(filepath.Join(destination, "src"), 0755))
				require.NoError(t, os.Symlink(outside, filepath.Join(destination, "src/alias")))
			case "cycle":
				require.NoError(t, os.Symlink("second", filepath.Join(destination, "alias")))
				require.NoError(t, os.Symlink("alias", filepath.Join(destination, "second")))
			}
			next := unsafeFixtureEnvelope(t, staging, names)
			_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
			assert.Error(t, err)
			contents, err := os.ReadFile(filepath.Join(destination, "seed"))
			require.NoError(t, err)
			assert.Equal(t, "original", string(contents))
			_, err = os.Lstat(filepath.Join(destination, "link"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			contents, err = os.ReadFile(filepath.Join(outside, "output"))
			require.NoError(t, err)
			assert.Equal(t, "protected", string(contents))
		})
	}
}

func TestRequestedSymlinkChainsAndDirectoryTransitions(t *testing.T) {
	destination, staging := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(destination, "src"), []byte("old donor file"), 0600))
	previous, err := BuildEnvelope(destination, []string{"src"}, DefaultLimits())
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(staging, "src"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "src/file"), []byte("source"), 0600))
	require.NoError(t, os.Symlink("src", filepath.Join(staging, "alias")))
	require.NoError(t, os.Symlink("alias/file", filepath.Join(staging, "link")))
	next, err := BuildEnvelope(staging, []string{"src/file", "alias", "link"}, DefaultLimits())
	require.NoError(t, err)
	_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
	require.NoError(t, err)
	contents, err := os.ReadFile(filepath.Join(destination, "link"))
	require.NoError(t, err)
	assert.Equal(t, "source", string(contents))
	before, err := os.Lstat(filepath.Join(destination, "link"))
	require.NoError(t, err)
	result, err := Reconcile(destination, staging, next, next, DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, 3, result.Unchanged)
	assert.Zero(t, result.Written)
	after, err := os.Lstat(filepath.Join(destination, "link"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	assert.Equal(t, before.ModTime(), after.ModTime())
}

func TestRequestedSymlinkCyclesAndChainBounds(t *testing.T) {
	for _, scenario := range []string{"cycle", "bound", "protected-via-alias"} {
		t.Run(scenario, func(t *testing.T) {
			destination, staging := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(destination, "seed"), []byte("original"), 0600))
			previous, err := BuildEnvelope(destination, []string{"seed"}, DefaultLimits())
			require.NoError(t, err)
			names := []string{"link"}
			switch scenario {
			case "cycle":
				require.NoError(t, os.Symlink("alias", filepath.Join(staging, "link")))
				require.NoError(t, os.Symlink("link", filepath.Join(staging, "alias")))
				names = append(names, "alias")
			case "protected-via-alias":
				require.NoError(t, os.Symlink("alias/output", filepath.Join(staging, "link")))
				require.NoError(t, os.Symlink("target", filepath.Join(staging, "alias")))
				names = append(names, "alias")
			case "bound":
				require.NoError(t, os.WriteFile(filepath.Join(staging, "file"), []byte("source"), 0600))
				names = append(names, "file")
				require.NoError(t, os.Symlink("a", filepath.Join(staging, "link")))
				for index := 0; index < 41; index++ {
					name := string(rune('a' + index))
					target := string(rune('a' + index + 1))
					if index == 40 {
						target = "file"
					}
					require.NoError(t, os.Symlink(target, filepath.Join(staging, name)))
					names = append(names, name)
				}
			}
			next := unsafeFixtureEnvelope(t, staging, names)
			_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
			require.Error(t, err)
			contents, err := os.ReadFile(filepath.Join(destination, "seed"))
			require.NoError(t, err)
			assert.Equal(t, "original", string(contents))
		})
	}
}

func TestRejectSymlinkDotDotAfterAliasBeforeWrites(t *testing.T) {
	destination, staging, outside := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(destination, "seed"), []byte("original"), 0600))
	previous, err := BuildEnvelope(destination, []string{"seed"}, DefaultLimits())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "seed"), []byte("changed"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "file"), []byte("lexical target"), 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "nested/sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "nested/sub/source"), []byte("source"), 0600))
	require.NoError(t, os.Symlink("nested/sub", filepath.Join(staging, "alias")))
	require.NoError(t, os.Symlink("alias/../file", filepath.Join(staging, "link")))
	require.NoError(t, os.Mkdir(filepath.Join(destination, "nested"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "output"), []byte("protected"), 0600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "output"), filepath.Join(destination, "nested/file")))
	next := unsafeFixtureEnvelope(t, staging, []string{"seed", "file", "nested/sub/source", "alias", "link"})
	_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
	assert.Error(t, err)
	contents, err := os.ReadFile(filepath.Join(destination, "seed"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(contents))
	_, err = os.Lstat(filepath.Join(destination, "link"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRequestedSymlinkDotDotUsesExpandedAlias(t *testing.T) {
	destination, staging := t.TempDir(), t.TempDir()
	previous, err := BuildEnvelope(destination, nil, DefaultLimits())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "nested/sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "file"), []byte("lexical"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "nested/file"), []byte("actual"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "nested/sub/source"), []byte("source"), 0600))
	require.NoError(t, os.Symlink("nested/sub", filepath.Join(staging, "alias")))
	require.NoError(t, os.Symlink("alias/../file", filepath.Join(staging, "link")))
	next, err := BuildEnvelope(staging, []string{"file", "nested/file", "nested/sub/source", "alias", "link"}, DefaultLimits())
	require.NoError(t, err)
	_, err = Reconcile(destination, staging, previous, next, DefaultLimits())
	require.NoError(t, err)
	contents, err := os.ReadFile(filepath.Join(destination, "link"))
	require.NoError(t, err)
	assert.Equal(t, "actual", string(contents))
}
