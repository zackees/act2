package sourcecheckout

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitFixture(t *testing.T, body string) (string, *git.Repository, FrozenSourceIdentity) {
	t.Helper()
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	require.NoError(t, err)
	identity := commitFixture(t, root, repo, body)
	return root, repo, identity
}
func commitFixture(t *testing.T, root string, repo *git.Repository, body string) FrozenSourceIdentity {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.rs"), []byte(body), 0600))
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("source.rs")
	require.NoError(t, err)
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Unix(12345, 0)}})
	require.NoError(t, err)
	commit, err := repo.CommitObject(hash)
	require.NoError(t, err)
	dataHash := sha256.Sum256([]byte(body))
	digest := sha256.Sum256([]byte("source.rs\x00f\x00" + hex.EncodeToString(dataHash[:]) + "\n"))
	return FrozenSourceIdentity{OriginalCommit: hash.String(), CheckoutCommit: hash.String(), GitTree: commit.TreeHash.String(), TreeDigest: hex.EncodeToString(digest[:])}
}

func TestFrozenCheckoutRejectsSealedUnprovenSource(t *testing.T) {
	for _, scenario := range []string{"wrong-tree", "wrong-checkout-commit", "wrong-frozen-digest", "tampered-source", "mode-mismatch", "dirty-original-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, identity := gitFixture(t, "authoritative")
			switch scenario {
			case "wrong-tree":
				identity.GitTree = "0123456789012345678901234567890123456789"
			case "wrong-checkout-commit":
				identity.CheckoutCommit = "0123456789012345678901234567890123456789"
			case "wrong-frozen-digest":
				identity.TreeDigest = "0123456789012345678901234567890123456789012345678901234567890123"
			case "tampered-source":
				require.NoError(t, os.WriteFile(filepath.Join(root, "source.rs"), []byte("forged"), 0600))
			case "mode-mismatch":
				require.NoError(t, os.Chmod(filepath.Join(root, "source.rs"), 0700))
			case "dirty-original-mismatch":
				identity.Dirty = true
				identity.OriginalCommit = "0123456789012345678901234567890123456789"
			}
			_, err := ReadFrozenCheckout(root, root, identity, DefaultLimits())
			assert.Error(t, err)
		})
	}
}

func TestFrozenCheckoutUsesIndependentGitAndSyntheticCommit(t *testing.T) {
	root, repo, original := gitFixture(t, "old")
	baseline := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(baseline, "source.rs"), []byte("old"), 0600))
	// Cache metadata is deliberately hostile and must never be opened.
	require.NoError(t, os.Symlink("/untrusted-object-store", filepath.Join(baseline, ".git")))
	requested := commitFixture(t, root, repo, "new")
	requested.Dirty = true
	requested.OriginalCommit = original.OriginalCommit
	donor, err := ReadFrozenCheckout(root, baseline, original, DefaultLimits())
	require.NoError(t, err)
	next, err := ReadFrozenCheckout(root, root, requested, DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, requested.CheckoutCommit, next.Envelope().Binding.SourceCommit)
	assert.NotEqual(t, requested.OriginalCommit, next.Envelope().Binding.SourceCommit)
	detached := donor.Envelope()
	detached.Entries[0].Path = "forged"
	assert.Equal(t, "source.rs", donor.Envelope().Entries[0].Path)
}

func TestFrozenCheckoutRejectsUnsupportedAndBoundedSources(t *testing.T) {
	t.Run("lfs", func(t *testing.T) {
		root, _, identity := gitFixture(t, "version https://git-lfs.github.com/spec/v1\n")
		_, err := ReadFrozenCheckout(root, root, identity, DefaultLimits())
		assert.Error(t, err)
	})
	t.Run("file-bound", func(t *testing.T) {
		root, _, identity := gitFixture(t, "large")
		limits := DefaultLimits()
		limits.FileBytes = 2
		_, err := ReadFrozenCheckout(root, root, identity, limits)
		assert.Error(t, err)
	})
	t.Run("alternates", func(t *testing.T) {
		root, _, identity := gitFixture(t, "source")
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".git/objects/info"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git/objects/info/alternates"), []byte("/untrusted"), 0600))
		_, err := ReadFrozenCheckout(root, root, identity, DefaultLimits())
		assert.Error(t, err)
	})
}

func TestFrozenDigestUsesComponentOrdering(t *testing.T) {
	entries := []Entry{{Path: "a-b", Kind: "file", SHA256: "second"}, {Path: "a/b", Kind: "file", SHA256: "first"}}
	expected := sha256.Sum256([]byte("a/b\x00f\x00first\na-b\x00f\x00second\n"))
	assert.Equal(t, hex.EncodeToString(expected[:]), frozenTreeDigest(entries))
}

func TestFrozenCheckoutRejectsUnboundedObjectDecoding(t *testing.T) {
	t.Run("external-common-store", func(t *testing.T) {
		root, _, identity := gitFixture(t, "source")
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git/commondir"), []byte("/untrusted"), 0600))
		_, err := ReadFrozenCheckout(root, root, identity, DefaultLimits())
		assert.Error(t, err)
	})

	t.Run("packed-store", func(t *testing.T) {
		root, _, identity := gitFixture(t, "source")
		pack := filepath.Join(root, ".git/objects/pack")
		require.NoError(t, os.MkdirAll(pack, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(pack, "untrusted.pack"), []byte("opaque"), 0600))
		_, err := ReadFrozenCheckout(root, root, identity, DefaultLimits())
		assert.Error(t, err)
	})
	t.Run("inflated-header", func(t *testing.T) {
		root, _, identity := gitFixture(t, "source")
		objectDir := filepath.Join(root, ".git/objects/aa")
		require.NoError(t, os.MkdirAll(objectDir, 0700))
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		_, err := writer.Write([]byte("blob 999999999999\x00"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		require.NoError(t, os.WriteFile(filepath.Join(objectDir, strings.Repeat("a", 38)), compressed.Bytes(), 0600))
		_, err = ReadFrozenCheckout(root, root, identity, DefaultLimits())
		assert.Error(t, err)
	})
}
