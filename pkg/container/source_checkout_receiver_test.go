package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nektos/act/pkg/sourcecheckout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These interfaces are test issuers, never a production authorization provider.
type testCheckoutSource struct{ receipt sourcecheckout.SourceReceipt }

func (source testCheckoutSource) FrozenSource(context.Context) (sourcecheckout.SourceReceipt, error) {
	return source.receipt, nil
}

type testCheckoutWriter struct{ grant sourcecheckout.BaselineGrant }

func (writer testCheckoutWriter) ApprovedBaseline(context.Context, string, string) (sourcecheckout.BaselineGrant, error) {
	return writer.grant, nil
}

type testCheckoutReceiver struct{ input SourceHandoffInput }

func (receiver testCheckoutReceiver) MaterializedHandoff(context.Context, string, string, bool) (SourceHandoffInput, error) {
	return receiver.input, nil
}

func commitCheckoutFixture(t *testing.T, root string, repository *git.Repository, files map[string]string) sourcecheckout.FrozenSourceIdentity {
	t.Helper()
	worktree, err := repository.Worktree()
	require.NoError(t, err)
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0600))
		_, err = worktree.Add(name)
		require.NoError(t, err)
	}
	return commitCurrentCheckoutFixture(t, root, repository)
}

func commitCurrentCheckoutFixture(t *testing.T, root string, repository *git.Repository) sourcecheckout.FrozenSourceIdentity {
	t.Helper()
	worktree, err := repository.Worktree()
	require.NoError(t, err)
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Unix(12345, 0)}})
	require.NoError(t, err)
	commit, err := repository.CommitObject(hash)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	entries := make(map[string]*object.File)
	paths := []string{}
	require.NoError(t, tree.Files().ForEach(func(file *object.File) error {
		entries[file.Name] = file
		paths = append(paths, file.Name)
		return nil
	}))
	sort.Strings(paths) // This fixture uses flat names only.
	digest := sha256.New()
	for _, name := range paths {
		file := entries[name]
		body, readErr := os.ReadFile(filepath.Join(root, name))
		if file.Mode == filemode.Symlink {
			target, linkErr := os.Readlink(filepath.Join(root, name))
			require.NoError(t, linkErr)
			_, err = digest.Write([]byte(name + "\x00link\x00" + target + "\n"))
		} else {
			require.NoError(t, readErr)
			content := sha256.Sum256(body)
			kind := "f"
			if file.Mode == filemode.Executable {
				kind = "x"
			}
			_, err = digest.Write([]byte(name + "\x00" + kind + "\x00" + hex.EncodeToString(content[:]) + "\n"))
		}
		require.NoError(t, err)
	}
	return sourcecheckout.FrozenSourceIdentity{OriginalCommit: hash.String(), CheckoutCommit: hash.String(), GitTree: commit.TreeHash.String(), TreeDigest: hex.EncodeToString(digest.Sum(nil))}
}

func TestActualCopyDirTrustedReceiverPreservesUnchangedSourceAndOutputs(t *testing.T) {
	source, owned := t.TempDir(), t.TempDir()
	destination := filepath.Join(owned, "workspace")
	require.NoError(t, os.Mkdir(destination, 0700))
	repository, err := git.PlainInit(source, false)
	require.NoError(t, err)
	_ = commitCheckoutFixture(t, source, repository, map[string]string{"same.rs": "same", "change.rs": "old", "gone.rs": "delete", "mode.sh": "script"})
	require.NoError(t, os.Symlink("same.rs", filepath.Join(source, "alias")))
	require.NoError(t, os.Symlink("same.rs", filepath.Join(source, "stable-link")))
	worktree, err := repository.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("alias")
	require.NoError(t, err)
	_, err = worktree.Add("stable-link")
	require.NoError(t, err)
	original := commitCurrentCheckoutFixture(t, source, repository)
	for name, body := range map[string]string{"same.rs": "same", "change.rs": "old", "gone.rs": "delete", "mode.sh": "script"} {
		require.NoError(t, os.WriteFile(filepath.Join(destination, name), []byte(body), 0600))
	}
	require.NoError(t, os.Symlink("same.rs", filepath.Join(destination, "alias")))
	require.NoError(t, os.Symlink("same.rs", filepath.Join(destination, "stable-link")))
	linkBefore, err := os.Lstat(filepath.Join(destination, "stable-link"))
	require.NoError(t, err)
	// Cached Git is hostile, and must not supply objects, hooks or configuration.
	require.NoError(t, os.Symlink("/untrusted-cache-git", filepath.Join(destination, ".git")))
	limits := sourcecheckout.DefaultLimits()
	donor, err := sourcecheckout.ReadFrozenCheckout(source, destination, original, limits)
	require.NoError(t, err)
	outputs := strings.Repeat("a", 64)
	envelope, err := donor.Envelope().Bind(sourcecheckout.Binding{SourceCommit: original.CheckoutCommit, GitTree: original.GitTree, OutputIdentity: outputs})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "change.rs"), []byte("new"), 0600))
	require.NoError(t, os.Chmod(filepath.Join(source, "mode.sh"), 0700))
	_, err = worktree.Remove("gone.rs")
	require.NoError(t, err)
	_, err = worktree.Remove("alias")
	require.NoError(t, err)
	require.NoError(t, os.Symlink("change.rs", filepath.Join(source, "alias")))
	for _, name := range []string{"alias", "change.rs", "mode.sh"} {
		_, err = worktree.Add(name)
		require.NoError(t, err)
	}
	requested := commitCurrentCheckoutFixture(t, source, repository)
	// Source configuration and hooks are never executed or restored. The safe
	// destination config must omit even valid source-side extension settings.
	require.NoError(t, os.WriteFile(filepath.Join(source, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tfilemode = true\n\thooksPath = /untrusted-hooks\n[filter \"untrusted\"]\n\tclean = forbidden-command\n"), 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(source, ".git", "hooks"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nexit 99\n"), 0700))
	grant := sourcecheckout.BaselineGrant{Source: original, GitRoot: source, MaterializedRoot: destination, CacheNamespace: "test-source-v1", OutputIdentity: outputs, PayloadSHA256: strings.Repeat("b", 64), PolicyCommit: original.CheckoutCommit, WorkflowCommit: original.CheckoutCommit, RunID: 1, Attempt: 1, JobID: 1}
	receipt := sourcecheckout.SourceReceipt{Identity: requested, GitRoot: source, MaterializedRoot: source, CacheNamespace: grant.CacheNamespace, OutputIdentity: outputs}
	receiver := testCheckoutReceiver{input: SourceHandoffInput{Candidate: sourcecheckout.BaselineCandidate{Envelope: envelope, PayloadSHA256: grant.PayloadSHA256}, Source: testCheckoutSource{receipt}, Writer: testCheckoutWriter{grant}, Limits: limits}}
	require.NoError(t, os.Mkdir(filepath.Join(destination, "target"), 0700))
	output := filepath.Join(destination, "target", "cached")
	require.NoError(t, os.WriteFile(output, []byte("protected"), 0400))
	same := filepath.Join(destination, "same.rs")
	old := time.Unix(1000, 123456789)
	changed := filepath.Join(destination, "change.rs")
	require.NoError(t, os.Chtimes(changed, old, old))
	require.NoError(t, os.Chtimes(filepath.Join(source, "change.rs"), old, old))
	require.NoError(t, os.Chtimes(same, old, old))
	require.NoError(t, os.Chtimes(output, old, old))
	before, err := os.Stat(same)
	require.NoError(t, err)
	outputBefore, err := os.Stat(output)
	require.NoError(t, err)
	environment := HostEnvironment{Path: destination, OwnedRoot: owned, Workdir: source, SourceReceiver: receiver}
	require.NoError(t, environment.CopyDir(destination, source+string(filepath.Separator)+".", false)(context.Background()))
	after, err := os.Stat(same)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	assert.Equal(t, before.ModTime(), after.ModTime())
	outputAfter, err := os.Stat(output)
	require.NoError(t, err)
	assert.True(t, os.SameFile(outputBefore, outputAfter))
	assert.Equal(t, outputBefore.ModTime(), outputAfter.ModTime())
	body, err := os.ReadFile(filepath.Join(destination, "change.rs"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(body))
	changedAfter, err := os.Stat(changed)
	require.NoError(t, err)
	assert.NotEqual(t, old, changedAfter.ModTime(), "changed same-size source with identical old mtime must materialize fresh")
	requestedChanged, err := os.Stat(filepath.Join(source, "change.rs"))
	require.NoError(t, err)
	assert.Equal(t, old, requestedChanged.ModTime(), "requested source stays frozen")
	_, err = os.Stat(filepath.Join(destination, "gone.rs"))
	assert.True(t, os.IsNotExist(err))
	mode, err := os.Stat(filepath.Join(destination, "mode.sh"))
	require.NoError(t, err)
	assert.NotZero(t, mode.Mode().Perm()&0111)
	alias, err := os.Readlink(filepath.Join(destination, "alias"))
	require.NoError(t, err)
	assert.Equal(t, "change.rs", alias)
	linkAfter, err := os.Lstat(filepath.Join(destination, "stable-link"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(linkBefore, linkAfter))
	assert.Equal(t, linkBefore.ModTime(), linkAfter.ModTime())
	head, err := os.ReadFile(filepath.Join(destination, ".git", "HEAD"))
	require.NoError(t, err)
	assert.Equal(t, requested.CheckoutCommit+"\n", string(head))
	config, err := os.ReadFile(filepath.Join(destination, ".git", "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(config), "hooksPath")
	assert.NotContains(t, string(config), "filter")
	_, err = os.Stat(filepath.Join(destination, ".git", "hooks"))
	assert.True(t, os.IsNotExist(err))
	gitDirectory, err := os.Lstat(filepath.Join(destination, ".git"))
	require.NoError(t, err)
	assert.True(t, gitDirectory.IsDir())
}
