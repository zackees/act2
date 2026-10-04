package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostDockerDoesNotBindOuterFilesystem(t *testing.T) {
	rc := &RunContext{Name: "host", Config: &Config{Workdir: "/outer/checkout"}, Run: &model.Run{Workflow: &model.Workflow{Name: "host"}}, JobContainer: &container.HostEnvironment{Path: "/outer/hostexecutor", ActPath: "/outer/act"}}
	binds, mounts := rc.GetBindsAndMounts()
	assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock"}, binds, "outer runner paths must transfer through Docker API instead of daemon-local binds")
	assert.Empty(t, mounts)
}

func TestHostDockerTransfersWorkspaceAndCommandFiles(t *testing.T) {
	root := t.TempDir()
	host := &container.HostEnvironment{}
	action := &container.HostEnvironment{}
	transfer := &hostDockerTransfer{host: host, paths: []hostDockerPath{
		{host: filepath.Join(root, "host-command"), action: filepath.Join(root, "action-command")},
		{host: filepath.Join(root, "host-workspace"), action: filepath.Join(root, "action-workspace")},
	}}
	for _, path := range transfer.paths {
		require.NoError(t, os.MkdirAll(path.host, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(path.host, "input"), []byte("input"), 0600))
	}
	require.NoError(t, transfer.stage(action)(context.Background()))
	for _, path := range transfer.paths {
		data, err := os.ReadFile(filepath.Join(path.action, "input"))
		require.NoError(t, err)
		assert.Equal(t, "input", string(data))
		require.NoError(t, os.WriteFile(filepath.Join(path.action, "output"), []byte("output"), 0600))
	}
	require.NoError(t, transfer.restore(action)(context.Background()))
	for _, path := range transfer.paths {
		data, err := os.ReadFile(filepath.Join(path.host, "output"))
		require.NoError(t, err)
		assert.Equal(t, "output", string(data))
		info, err := os.Stat(filepath.Join(path.host, "output"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
}
func TestHostDockerMapsOnlyRuntimePaths(t *testing.T) {
	host := &container.HostEnvironment{Path: "/outer/executor", ActPath: "/outer/act"}
	rc := &RunContext{Config: &Config{Workdir: "/checkout"}, JobContainer: host}
	assert.Equal(t, "/var/run/act/workflow/output", hostDockerEnv(rc, "GITHUB_OUTPUT", "/outer/act/workflow/output"))
	assert.Equal(t, "/outer/act/workflow/output", hostDockerEnv(rc, "INPUT_ARGUMENT", "/outer/act/workflow/output"))
}
