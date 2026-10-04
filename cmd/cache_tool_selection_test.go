package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
)

func TestToolSelectionCommandsKeepPriorWarmInstalls(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux Docker tool generations")
	}
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), []byte("warm"), 0600))
	object := artifactcache.PublishToolSnapshot(context.Background(), source, store, 100)
	require.False(t, object.Partial, object.Error)
	run := func(args ...string) []byte {
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append([]string{"cache"}, args...))
		require.NoError(t, root.Execute())
		return out.Bytes()
	}
	var previous artifactcache.ToolGenerationUpdateReport
	for index, path := range []string{"Go/1/x64", "Node/2/x64"} {
		spec := artifactcache.ToolGenerationSpec{SchemaVersion: 1, Installs: []artifactcache.ToolGenerationInstall{{Path: path, ObjectID: object.ID}}}
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		manifest := filepath.Join(base, "updates.json")
		require.NoError(t, os.WriteFile(manifest, data, 0600))
		var report artifactcache.ToolGenerationUpdateReport
		args := []string{"tool-update", "--manifest", manifest, "--cache-server-path", store, "--max-bytes", "100", "--apply"}
		if index == 0 {
			args = append(args, "--initialize")
		}
		require.NoError(t, json.Unmarshal(run(args...), &report))
		require.False(t, report.Partial, report.Error)
		require.True(t, report.Selected)
		if previous.Generation.ID != "" {
			_, err := os.Stat(filepath.Join(previous.Generation.Destination, "tree", "Node"))
			require.True(t, os.IsNotExist(err))
		}
		previous = report
	}
	var current artifactcache.ToolGenerationSelection
	require.NoError(t, json.Unmarshal(run("tool-current", "--cache-server-path", store, "--max-bytes", "100"), &current))
	require.Equal(t, previous.Generation.ID, current.ID)
	for _, path := range []string{"Go/1/x64", "Node/2/x64"} {
		payload, err := os.ReadFile(filepath.Join(previous.Generation.Destination, "tree", path, "payload"))
		require.NoError(t, err)
		require.Equal(t, "warm", string(payload))
	}
}
