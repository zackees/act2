package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nektos/act/pkg/common/git"
	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/require"
)

func initializeLegacyCheckout(t *testing.T, source string) string {
	t.Helper()
	repo, err := gogit.PlainInit(source, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "code.js"), []byte("fixture"), 0600))
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("code.js")
	require.NoError(t, err)
	pin, err := worktree.Commit("fixture", &gogit.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(1, 0)}})
	require.NoError(t, err)
	return pin.String()
}

func legacyCheckoutFixture(t *testing.T) (*stepActionRemote, git.NewGitCloneExecutorInput) {
	t.Helper()
	source := t.TempDir()
	pin := initializeLegacyCheckout(t, source)
	input := git.NewGitCloneExecutorInput{URL: source, Ref: pin, Dir: filepath.Join(t.TempDir(), "dir")}
	require.NoError(t, git.NewGitCloneExecutor(input)(context.Background()))
	step := &stepActionRemote{Step: &model.Step{Uses: "fixture/action@v1"}, RunContext: &RunContext{Config: &Config{}}, resolvedSha: pin}
	return step, input
}

// ci.yml#362: a clone must not rewrite the worktree a sibling is copying.
func TestLegacyActionCopyExcludesCheckout(t *testing.T) {
	step, input := legacyCheckoutFixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	cm := &containerMock{}
	cm.On("CopyDir", "/action/", input.Dir+"/", false).Return(func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	step.RunContext.JobContainer = cm
	go func() { done <- maybeCopyToActionDir(context.Background(), step, input.Dir, "", "/action") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("copy never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cloneErr := git.NewGitCloneExecutor(input)(ctx)
	close(release)
	require.NoError(t, <-done)
	require.ErrorIs(t, cloneErr, context.DeadlineExceeded, "checkout admission must wait for the active cache reader")
	cm.AssertExpectations(t)
}

func TestLegacyActionCopyRejectsChangedRevision(t *testing.T) {
	step, input := legacyCheckoutFixture(t)
	repo, err := gogit.PlainOpen(input.Dir)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(input.Dir, "code.js"), []byte("changed revision"), 0600))
	_, err = worktree.Add("code.js")
	require.NoError(t, err)
	_, err = worktree.Commit("changed", &gogit.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(2, 0)}})
	require.NoError(t, err)
	cm := &containerMock{}
	cm.On("CopyDir", "/action/", input.Dir+"/", false).Return(func(context.Context) error { return nil })
	step.RunContext.JobContainer = cm
	err = maybeCopyToActionDir(context.Background(), step, input.Dir, "", "/action")
	require.ErrorContains(t, err, "checkout changed")
	cm.AssertNotCalled(t, "CopyDir", "/action/", input.Dir+"/", false)
}
