package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
)

// Docker actions use copies of the host executor's private trees. Their paths
// remain identical on Linux, including every GitHub file-command destination.
// No bind requires the daemon to share the executor's filesystem namespace.
func (rc *RunContext) dockerActionPath(value string) string {
	host, ok := rc.JobContainer.(*container.HostEnvironment)
	if !ok {
		return value
	}
	ext := container.LinuxContainerEnvironmentExtensions{}
	for _, root := range []string{host.Path, host.ActPath} {
		value = strings.ReplaceAll(value, root, ext.ToContainerPath(root))
	}
	return value
}

func (rc *RunContext) dockerActionStrings(values []string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = rc.dockerActionPath(value)
	}
	return result
}

func (rc *RunContext) startDockerAction(action container.Container) common.Executor {
	host, ok := rc.JobContainer.(*container.HostEnvironment)
	if !ok {
		return action.Start(true)
	}
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return action.Start(true)(ctx)
		}
		for _, root := range []string{host.Path, host.ActPath} {
			archive, err := host.GetOwnedTreeArchive(ctx, root)
			if err != nil {
				return err
			}
			copyErr := action.CopyTarStream(ctx, rc.dockerActionPath(root), archive)
			if err = errors.Join(copyErr, archive.Close()); err != nil {
				return err
			}
		}
		runErr := action.Start(true)(ctx)
		// Failed/cancelled actions still have outputs and workspace changes. Bound
		// reconciliation independently of cancellation, before container cleanup.
		copyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		for _, root := range []string{host.Path, host.ActPath} {
			archive, err := action.GetContainerArchive(copyCtx, rc.dockerActionPath(root)+"/.")
			if err != nil {
				return errors.Join(runErr, err)
			}
			copyErr := host.RestoreOwnedTree(copyCtx, root, archive)
			if err = errors.Join(copyErr, archive.Close()); err != nil {
				return errors.Join(runErr, err)
			}
		}
		return runErr
	}
}
