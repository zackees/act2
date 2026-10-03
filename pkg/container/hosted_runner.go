//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	_ "embed"
	"fmt"
	"path"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/nektos/act/pkg/common"
)

const hostedRunnerUser = "actrunner"
const hostedRunnerHome = "/home/actrunner"

//go:embed hosted_runner.sh
var hostedRunnerSetup string

func (cr *containerReference) defaultExecUser() string {
	if cr.input.HostedRunner {
		return hostedRunnerUser
	}
	return ""
}

func (cr *containerReference) provisionHostedRunner() common.Executor {
	return func(ctx context.Context) error {
		if !cr.input.HostedRunner {
			return nil
		}
		inspected, err := cr.cli.ContainerInspect(ctx, cr.id, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect hosted runner mounts: %w", err)
		}
		cr.protectedPaths = nil
		for _, mounted := range inspected.Container.Mounts {
			if mounted.Type == mount.TypeBind {
				cr.protectedPaths = append(cr.protectedPaths, mounted.Destination)
			}
		}
		if !cr.mayChown(hostedRunnerHome) {
			return fmt.Errorf("hosted runner cannot provision bind-mounted HOME %s", hostedRunnerHome)
		}
		// Account databases, their locks/backups and sudo configuration must
		// stay on the job's private filesystem during account provisioning.
		if !cr.mayChown("/etc") || !cr.mayChown("/var/mail") || !cr.mayChown("/var/spool/mail") {
			return fmt.Errorf("hosted runner cannot provision host-bound account configuration")
		}
		toolcache, actions := "/opt/hostedtoolcache", "/var/run/act"
		if !cr.mayChown(toolcache) {
			toolcache = ""
		}
		if !cr.mayChown(actions) {
			actions = ""
		}
		return cr.Exec([]string{"sh", "-ec", hostedRunnerSetup, "act-hosted-runner", cr.input.WorkingDir, toolcache, actions}, nil, "0", "/")(ctx)
	}
}

// Paths bind-mounted from outside the job must retain their ownership.
func (cr *containerReference) mayChown(dest string) bool {
	dest = path.Clean(dest)
	for _, target := range cr.protectedPaths {
		if pathsOverlap(dest, target) {
			return false
		}
	}
	for _, bind := range cr.input.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 {
			continue
		}
		target := parts[len(parts)-1]
		if !strings.HasPrefix(target, "/") {
			target = parts[len(parts)-2]
		}
		target = path.Clean(target)
		if pathsOverlap(dest, target) {
			return false
		}
	}
	return true
}

func pathsOverlap(first, second string) bool {
	first, second = path.Clean(first), path.Clean(second)
	return first == "/" || second == "/" || first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/")
}
