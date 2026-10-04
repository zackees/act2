package git

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/stretchr/testify/require"
)

// Fixture directories are deliberately retained in the isolated test runner.
// Its owning harness removes the disposable container, not a test-owned host tree.
func immutableFixtureDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "act2-immutable-git-")
	require.NoError(t, err)
	return dir
}

type immutableFixture struct {
	dir     string
	repo    *gogit.Repository
	commits []plumbing.Hash
}

func newImmutableFixture(t *testing.T) immutableFixture {
	t.Helper()
	dir := immutableFixtureDirectory(t)
	repo, err := gogit.PlainInit(filepath.Join(dir, "fixture.git"), false)
	require.NoError(t, err)
	config, err := repo.Config()
	require.NoError(t, err)
	config.Raw.SetOption("uploadpack", "", "allowReachableSHA1InWant", "true")
	require.NoError(t, repo.SetConfig(config))
	fixture := immutableFixture{dir: dir, repo: repo}
	for i := range 24 {
		fixture.commits = append(fixture.commits, fixture.commit(t, fmt.Sprintf("revision-%02d\n", i)))
	}
	return fixture
}

func (fixture immutableFixture) commit(t *testing.T, contents string) plumbing.Hash {
	t.Helper()
	file := filepath.Join(fixture.dir, "fixture.git", "action.yml")
	require.NoError(t, os.WriteFile(file, []byte(contents), 0o600))
	worktree, err := fixture.repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("action.yml")
	require.NoError(t, err)
	hash, err := worktree.Commit(contents, &gogit.CommitOptions{Author: &object.Signature{
		Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(1, 0),
	}})
	require.NoError(t, err)
	return hash
}

func (fixture immutableFixture) submoduleCommit(t *testing.T) plumbing.Hash {
	t.Helper()
	worktree, err := fixture.repo.Worktree()
	require.NoError(t, err)
	modules := "[submodule \"dependency\"]\n\tpath = dependency\n\turl = ../dependency.git\n"
	require.NoError(t, os.WriteFile(filepath.Join(fixture.dir, "fixture.git", ".gitmodules"), []byte(modules), 0o600))
	_, err = worktree.Add(".gitmodules")
	require.NoError(t, err)
	idx, err := fixture.repo.Storer.Index()
	require.NoError(t, err)
	idx.Entries = append(idx.Entries, &index.Entry{Name: "dependency", Mode: filemode.Submodule, Hash: fixture.commits[7]})
	require.NoError(t, fixture.repo.Storer.SetIndex(idx))
	hash, err := worktree.Commit("submodule metadata", &gogit.CommitOptions{Author: &object.Signature{
		Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(2, 0),
	}})
	require.NoError(t, err)
	return hash
}

type immutableRequest struct {
	wants []plumbing.Hash
	depth packp.Depth
}

type immutableHTTP struct {
	server          *httptest.Server
	mu              sync.Mutex
	requests        int
	uploads         []immutableRequest
	rejectShallow   bool
	status          int
}

func newImmutableHTTP(t *testing.T, fixture immutableFixture) *immutableHTTP {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Root: "/git", Dir: fixture.dir,
		Env: []string{"GIT_PROJECT_ROOT=" + fixture.dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
		Stderr: io.Discard,
	}
	state := &immutableHTTP{}
	state.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.requests++
		status, reject := state.status, state.rejectShallow
		state.mu.Unlock()
		if status != 0 {
			http.Error(w, "fixture rejection", status)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			request := packp.NewUploadPackRequest()
			if decodeErr := request.Decode(bytes.NewReader(body)); decodeErr != nil {
				http.Error(w, "invalid upload-pack request", http.StatusBadRequest)
				return
			}
			state.mu.Lock()
			state.uploads = append(state.uploads, immutableRequest{wants: append([]plumbing.Hash(nil), request.Wants...), depth: request.Depth})
			state.mu.Unlock()
			if reject && !request.Depth.IsZero() {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				message := "ERR upload-pack: not our ref\n"
				_, _ = fmt.Fprintf(w, "%04x%s", len(message)+4, message)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(state.server.Close)
	return state
}

func (state *immutableHTTP) url() string { return state.server.URL + "/git/fixture.git" }

func (state *immutableHTTP) snapshot() (int, []immutableRequest) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.requests, append([]immutableRequest(nil), state.uploads...)
}

func (state *immutableHTTP) configure(status int, rejectShallow bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.status, state.rejectShallow = status, rejectShallow
}

func requireImmutableCheckout(t *testing.T, dir string, hash plumbing.Hash, contents string) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, hash, head.Hash())
	data, err := os.ReadFile(filepath.Join(dir, "action.yml"))
	require.NoError(t, err)
	require.Equal(t, contents, string(data))
	return repo
}

func seedImmutableLooseCheckout(t *testing.T, fixture immutableFixture, dir, url string, pin plumbing.Hash) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	objects, err := fixture.repo.Storer.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	require.NoError(t, objects.ForEach(func(obj plumbing.EncodedObject) error {
		_, copyErr := repo.Storer.SetEncodedObject(obj)
		return copyErr
	}))
	_, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, worktree.Checkout(&gogit.CheckoutOptions{Hash: pin}))
	return repo
}
