package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
)

type hostDockerPath struct{ host, action string }
type hostDockerTransfer struct {
	host   *container.HostEnvironment
	paths  []hostDockerPath
	staged bool
}

func newHostDockerTransfer(rc *RunContext) *hostDockerTransfer {
	host, ok := rc.JobContainer.(*container.HostEnvironment)
	transfer := &hostDockerTransfer{host: host}
	if ok {
		ext := container.LinuxContainerEnvironmentExtensions{}
		transfer.paths = []hostDockerPath{
			{host: host.GetActPath(), action: ext.GetActPath()},
			{host: host.ToContainerPath(rc.Config.Workdir), action: ext.ToContainerPath(rc.Config.Workdir)},
		}
	}
	return transfer
}
func copyHostDockerArchive(ctx context.Context, source, destination container.Container, src, dst string) error {
	archive, err := source.GetContainerArchive(ctx, src+"/.")
	if err != nil {
		return err
	}
	defer archive.Close()
	return destination.CopyTarStream(ctx, dst, archive)
}
func (transfer *hostDockerTransfer) stage(action container.Container) common.Executor {
	return func(ctx context.Context) error {
		if transfer.host == nil || common.Dryrun(ctx) {
			return nil
		}
		for _, path := range transfer.paths {
			if err := copyHostDockerArchive(ctx, transfer.host, action, path.host, path.action); err != nil {
				return fmt.Errorf("stage host Docker action files: %w", err)
			}
		}
		transfer.staged = true
		return nil
	}
}
func (transfer *hostDockerTransfer) restore(action container.Container) common.Executor {
	return func(ctx context.Context) error {
		if !transfer.staged {
			return nil
		}
		return restoreHostDockerFiles(ctx, action, transfer.paths)
	}
}
func hostDockerEnv(rc *RunContext, key, value string) string {
	_, ok := rc.JobContainer.(*container.HostEnvironment)
	if !ok {
		return value
	}
	switch key {
	case "GITHUB_WORKSPACE", "GITHUB_ACTION_PATH", "GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STATE", "GITHUB_PATH", "GITHUB_STEP_SUMMARY":
		for _, path := range newHostDockerTransfer(rc).paths {
			if value == path.host {
				return path.action
			}
			if strings.HasPrefix(value, path.host+"/") {
				return path.action + strings.TrimPrefix(value, path.host)
			}
		}
	}
	return value
}
