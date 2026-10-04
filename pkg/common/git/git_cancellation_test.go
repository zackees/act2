package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// These bounds diagnose cancellation failures; they do not change production
// deadlines or the integration suite's existing Go timeout.
func TestGitCloneAdmissionCancellation(t *testing.T) {
	cloneLock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewGitCloneExecutor(NewGitCloneExecutorInput{Dir: filepath.Join(t.TempDir(), "absent")})(ctx)
	}()
	select {
	case err := <-done:
		cloneLock.Unlock()
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		cloneLock.Unlock()
		<-done // Release the old implementation before returning a RED failure.
		t.Fatal("canceled clone admission waited for the global lock")
	}
}

type cancellationRepository struct {
	dir  string
	hash plumbing.Hash
}

func newCancellationRepository(t *testing.T) cancellationRepository {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("fixture\n"), 0o600))
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("file")
	require.NoError(t, err)
	hash, err := worktree.Commit("fixture", &gogit.CommitOptions{Author: &object.Signature{
		Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(1, 0),
	}})
	require.NoError(t, err)
	return cancellationRepository{dir: dir, hash: hash}
}

func (fixture cancellationRepository) input(t *testing.T, url string) NewGitCloneExecutorInput {
	t.Helper()
	repo, err := gogit.PlainOpen(fixture.dir)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	require.NoError(t, err)
	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.Branches["master"] = &config.Branch{Name: "master", Remote: "origin", Merge: "refs/heads/master"}
	require.NoError(t, repo.SetConfig(cfg))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference("refs/remotes/origin/master", fixture.hash)))
	return NewGitCloneExecutorInput{Dir: fixture.dir, URL: url, Ref: fixture.hash.String()}
}

func writeAdvertisement(w http.ResponseWriter, hash plumbing.Hash) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	for _, line := range []string{
		"# service=git-upload-pack\n", "",
		hash.String() + " HEAD\x00symref=HEAD:refs/heads/master\n",
		hash.String() + " refs/heads/master\n", "",
	} {
		if line == "" {
			_, _ = fmt.Fprint(w, "0000")
		} else {
			_, _ = fmt.Fprintf(w, "%04x%s", len(line)+4, line)
		}
	}
}

func TestGitRefreshCancellation(t *testing.T) {
	for _, phase := range []string{"fetch", "pull"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newCancellationRepository(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "pull" && requests.Add(1) == 1 {
					writeAdvertisement(w, fixture.hash)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/info/refs") {
					http.Error(w, "unexpected Git fixture request", http.StatusBadRequest)
					return
				}
				select {
				case <-entered:
				default:
					close(entered)
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			input := fixture.input(t, server.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- NewGitCloneExecutor(input)(ctx) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("executor did not reach stalled %s: %v", phase, err)
			case <-time.After(3 * time.Second):
				t.Fatalf("executor did not reach stalled %s", phase)
			}
			cancel()
			select {
			case err := <-done:
				require.True(t, errors.Is(err, context.Canceled), "cancellation was swallowed: %v", err)
			case <-time.After(time.Second):
				t.Fatalf("%s ignored caller cancellation", phase)
			}
		})
	}
}
