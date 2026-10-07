package runner

import (
	"context"
	"fmt"
	"os"

	gogit "github.com/go-git/go-git/v5"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/common/git"
)

// A legacy cache checkout is mutable. Validate and consume it under the same
// gate as clone/reset, then release the gate before running any action code.
func withLegacyActionCheckout(ctx context.Context, step actionStep, actionDir string, read common.Executor) error {
	return git.WithGitCacheLock(ctx, func(ctx context.Context) error {
		remote, ok := step.(*stepActionRemote)
		if !ok || remote.resolvedSha == "" {
			return fmt.Errorf("remote action checkout has no recorded revision")
		}
		head, err := legacyActionHead(actionDir)
		if err != nil {
			return err
		}
		if head != remote.resolvedSha {
			return fmt.Errorf("remote action checkout changed after manifest resolution")
		}
		return read(ctx)
	})
}

func (sar *stepActionRemote) readLegacyAction(ctx context.Context, actionDir string, reader actionYamlReader) error {
	return git.WithGitCacheLock(ctx, func(ctx context.Context) error {
		head, err := legacyActionHead(actionDir)
		if err != nil {
			return err
		}
		sar.resolvedSha = head
		action, err := sar.readAction(ctx, sar.Step, actionDir, sar.remoteAction.Path, reader, os.WriteFile)
		sar.action = action
		return err
	})
}

func legacyActionHead(actionDir string) (string, error) {
	repo, err := gogit.PlainOpen(actionDir)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	return head.Hash().String(), nil
}
