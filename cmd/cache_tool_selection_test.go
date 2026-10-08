package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	var state struct {
		SchemaVersion int                                   `json:"schema_version"`
		ID            string                                `json:"id"`
		Installs      []artifactcache.ToolGenerationInstall `json:"installs"`
	}
	require.NoError(t, json.Unmarshal(run("tool-current", "--installs", "--cache-server-path", store, "--max-bytes", "100"), &state))
	require.Equal(t, current.ID, state.ID)
	require.Equal(t, 1, state.SchemaVersion)
	require.Equal(t, []artifactcache.ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}, {Path: "Node/2/x64", ObjectID: object.ID}}, state.Installs)

	for _, path := range []string{"Go/1/x64", "Node/2/x64"} {
		payload, err := os.ReadFile(filepath.Join(previous.Generation.Destination, "tree", path, "payload"))
		require.NoError(t, err)
		require.Equal(t, "warm", string(payload))
	}
}

func TestToolSelectionReplaceCommandUsesExactInstallSet(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux Docker tool generations")
	}
	ctx := context.Background()
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), []byte("warm"), 0600))
	object := artifactcache.PublishToolSnapshot(ctx, source, store, 4)
	require.False(t, object.Partial, object.Error)
	expected := ""
	for index, path := range []string{"Go/1/x64", "Go/2/x64"} {
		spec := artifactcache.ToolGenerationSpec{SchemaVersion: 1, Installs: []artifactcache.ToolGenerationInstall{{Path: path, ObjectID: object.ID}}}
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		manifest := filepath.Join(base, "updates.json")
		require.NoError(t, os.WriteFile(manifest, data, 0600))
		args := []string{"cache", "tool-update", "--manifest", manifest, "--cache-server-path", store, "--max-bytes", "4", "--apply"}
		if index == 0 {
			args = append(args, "--initialize")
		} else {
			args = append(args, "--replace", "--expected-generation", expected)
		}
		root := createRootCommand(ctx, &Input{}, "test")
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetArgs(args)
		require.NoError(t, root.Execute())
		var report artifactcache.ToolGenerationUpdateReport
		require.NoError(t, json.Unmarshal(output.Bytes(), &report))
		require.False(t, report.Partial, report.Error)
		require.True(t, report.Selected)
		expected = report.Generation.ID
		if index == 1 {
			_, err := os.Lstat(filepath.Join(report.Generation.Destination, "tree", "Go/1/x64"))
			require.True(t, os.IsNotExist(err))
			payload, err := os.ReadFile(filepath.Join(report.Generation.Destination, "tree", path, "payload"))
			require.NoError(t, err)
			require.Equal(t, "warm", string(payload))
		}
	}
}

func TestToolSelectionReplaceAndInitializeAreExclusive(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "objects")
	root := createRootCommand(context.Background(), &Input{}, "test")
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"cache", "tool-update", "--initialize", "--replace", "--expected-generation", strings.Repeat("0", 64), "--apply", "--cache-server-path", store})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "initialize")
	require.Contains(t, err.Error(), "replace")
	_, err = os.Lstat(store)
	require.True(t, os.IsNotExist(err), "invalid selection mode cannot create cache authority")
}
