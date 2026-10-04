package git

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/stretchr/testify/require"
)

func TestGitImmutableColdAcquisition(t *testing.T) {
	for _, revision := range []int{23, 7} {
		t.Run(fmt.Sprintf("revision-%d", revision), func(t *testing.T) {
			fixture := newImmutableFixture(t)
			server := newImmutableHTTP(t, fixture)
			dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
			pin := fixture.commits[revision]
			input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin.String(), Dir: dir}
			require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
			repo := requireImmutableCheckout(t, dir, pin, fmt.Sprintf("revision-%02d\n", revision))
			_, uploads := server.snapshot()
			require.Len(t, uploads, 1, "a pinned acquisition must not perform an all-ref refresh or pull")
			require.Equal(t, []plumbing.Hash{pin}, uploads[0].wants, "request the independent fixture pin, not advertised branch heads")
			require.Equal(t, packp.DepthCommits(1), uploads[0].depth)
			commits, err := repo.CommitObjects()
			require.NoError(t, err)
			count := 0
			require.NoError(t, commits.ForEach(func(commit *object.Commit) error {
				count++
				require.Equal(t, pin, commit.Hash, "unrelated history must not be transferred")
				return nil
			}))
			require.Equal(t, 1, count)
		})
	}
}

func TestGitImmutableWarmAcquisitionDoesNotRefresh(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	pin := fixture.commits[7]
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin.String(), Dir: dir}
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	before, _ := server.snapshot()
	server.configure(http.StatusServiceUnavailable, false)
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()), "an immutable verified pin does not require network availability")
	after, _ := server.snapshot()
	require.Equal(t, before, after, "warm acquisition must make zero remote requests")
	requireImmutableCheckout(t, dir, pin, "revision-07\n")
}

func TestGitImmutableUnsupportedShallowFallback(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	server.configure(0, true)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	pin := fixture.commits[7]
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin.String(), Dir: dir}
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	requireImmutableCheckout(t, dir, pin, "revision-07\n")
	_, uploads := server.snapshot()
	require.Len(t, uploads, 2, "one unsupported targeted request permits only one legacy fallback")
	require.Equal(t, packp.DepthCommits(1), uploads[0].depth)
	require.Equal(t, []plumbing.Hash{pin}, uploads[0].wants)
	require.True(t, uploads[1].depth.IsZero(), "legacy fallback may obtain full history")
}

func TestGitImmutableAuthenticationDoesNotFallback(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	server.configure(http.StatusUnauthorized, false)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: fixture.commits[7].String(), Dir: dir, Token: "fixture-only"}
	require.Error(t, NewGitCloneExecutor(input)(context.Background()))
	requests, uploads := server.snapshot()
	require.Equal(t, 1, requests, "authentication failure must not retry through a broad clone")
	require.Empty(t, uploads)
}

func TestGitImmutableCanceledAcquisition(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: fixture.commits[7].String(), Dir: dir}
	require.ErrorIs(t, NewGitCloneExecutor(input)(ctx), context.Canceled)
	requests, uploads := server.snapshot()
	require.Zero(t, requests)
	require.Empty(t, uploads)
}

func TestGitImmutableOriginMismatchNeverReusesPin(t *testing.T) {
	fixture := newImmutableFixture(t)
	first := newImmutableHTTP(t, fixture)
	second := newImmutableHTTP(t, fixture)
	second.configure(http.StatusUnauthorized, false)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	input := NewGitCloneExecutorInput{URL: first.url(), Ref: fixture.commits[7].String(), Dir: dir}
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	input.URL = second.url()
	require.Error(t, NewGitCloneExecutor(input)(context.Background()), "same commit bytes from another origin do not authorize a hit")
	requests, _ := second.snapshot()
	require.Equal(t, 1, requests)
}

func TestGitImmutableCorruptCommitDoesNotFallback(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	// Seed a local loose-object checkout independently of the executor so the
	// corruption targets a known commit object rather than a production cache layout.
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	repo, err := gogit.PlainClone(dir, false, &gogit.CloneOptions{URL: filepath.Join(fixture.dir, "fixture.git")})
	require.NoError(t, err)
	config, err := repo.Config()
	require.NoError(t, err)
	config.Remotes["origin"].URLs = []string{server.url()}
	require.NoError(t, repo.SetConfig(config))
	pin := fixture.commits[7].String()
	objectPath := filepath.Join(dir, ".git", "objects", pin[:2], pin[2:])
	// Local clone can pack objects; write a corrupt loose object which takes
	// precedence over the valid packed copy when resolving this commit.
	require.NoError(t, os.MkdirAll(filepath.Dir(objectPath), 0o700))
	require.NoError(t, os.WriteFile(objectPath, []byte("invalid zlib object"), 0o600))
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin, Dir: dir}
	var acquisitionErr error
	require.NotPanics(t, func() { acquisitionErr = NewGitCloneExecutor(input)(context.Background()) })
	require.Error(t, acquisitionErr)
	requests, uploads := server.snapshot()
	require.Zero(t, requests, "corrupt cached objects are an error, not authority for broad fallback")
	require.Empty(t, uploads)
}

func TestGitImmutableCorruptTreeDoesNotRefresh(t *testing.T) {
	fixture := newImmutableFixture(t)
	server := newImmutableHTTP(t, fixture)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	repo, err := gogit.PlainClone(dir, false, &gogit.CloneOptions{URL: filepath.Join(fixture.dir, "fixture.git")})
	require.NoError(t, err)
	config, err := repo.Config()
	require.NoError(t, err)
	config.Remotes["origin"].URLs = []string{server.url()}
	require.NoError(t, repo.SetConfig(config))
	pin := fixture.commits[7]
	commit, err := repo.CommitObject(pin)
	require.NoError(t, err)
	treeHash := commit.TreeHash.String()
	objectPath := filepath.Join(dir, ".git", "objects", treeHash[:2], treeHash[2:])
	require.NoError(t, os.MkdirAll(filepath.Dir(objectPath), 0o700))
	require.NoError(t, os.WriteFile(objectPath, []byte("invalid tree zlib object"), 0o600))
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin.String(), Dir: dir}
	var acquisitionErr error
	require.NotPanics(t, func() { acquisitionErr = NewGitCloneExecutor(input)(context.Background()) })
	require.Error(t, acquisitionErr)
	requests, uploads := server.snapshot()
	require.Zero(t, requests, "a commit with unreadable tree data is not a verified warm hit")
	require.Empty(t, uploads)
}

func TestGitImmutablePreservesSubmoduleMetadata(t *testing.T) {
	fixture := newImmutableFixture(t)
	pin := fixture.submoduleCommit(t)
	server := newImmutableHTTP(t, fixture)
	dir := filepath.Join(immutableFixtureDirectory(t), "checkout")
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: pin.String(), Dir: dir}
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	repo := requireImmutableCheckout(t, dir, pin, "revision-23\n")
	commit, err := repo.CommitObject(pin)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	entry, err := tree.FindEntry("dependency")
	require.NoError(t, err)
	require.Equal(t, filemode.Submodule, entry.Mode)
	require.Equal(t, fixture.commits[7], entry.Hash)
	modules, err := os.ReadFile(filepath.Join(dir, ".gitmodules"))
	require.NoError(t, err)
	require.Equal(t, "[submodule \"dependency\"]\n\tpath = dependency\n\turl = ../dependency.git\n", string(modules))
}

func TestGitMutableBranchAndTagAcquisitionRemainFresh(t *testing.T) {
	fixture := newImmutableFixture(t)
	_, err := fixture.repo.CreateTag("fixture-tag", fixture.commits[7], nil)
	require.NoError(t, err)
	server := newImmutableHTTP(t, fixture)
	branchDir := filepath.Join(immutableFixtureDirectory(t), "branch")
	input := NewGitCloneExecutorInput{URL: server.url(), Ref: "master", Dir: branchDir}
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	before, _ := server.snapshot()
	newHead := fixture.commit(t, "new branch head\n")
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	after, _ := server.snapshot()
	require.Greater(t, after, before, "a mutable branch must refresh its origin")
	requireImmutableCheckout(t, branchDir, newHead, "new branch head\n")
	tagDir := filepath.Join(immutableFixtureDirectory(t), "tag")
	input.Ref, input.Dir = "fixture-tag", tagDir
	require.NoError(t, NewGitCloneExecutor(input)(context.Background()))
	requireImmutableCheckout(t, tagDir, fixture.commits[7], "revision-07\n")
}
