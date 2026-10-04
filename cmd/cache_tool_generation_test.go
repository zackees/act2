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

func TestToolGenerationReusesClosedObjectsAcrossSuccessors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux Docker tool generations")
	}
	base := t.TempDir()
	store := filepath.Join(base, "objects")
	objects := make([]artifactcache.ToolSnapshotReport, 2)
	for i, name := range []string{"first", "second"} {
		source := filepath.Join(base, name)
		require.NoError(t, os.Mkdir(source, 0755))
		require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
		require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte(name), 0600))
		objects[i] = artifactcache.PublishToolSnapshot(context.Background(), source, store, 100)
		require.False(t, objects[i].Partial, objects[i].Error)
	}
	type install struct {
		Path     string `json:"path"`
		ObjectID string `json:"object_id"`
	}
	publish := func(installs []install) (string, string, bool) {
		spec := struct {
			SchemaVersion int       `json:"schema_version"`
			Installs      []install `json:"installs"`
		}{1, installs}
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		manifest := filepath.Join(base, "generation.json")
		require.NoError(t, os.WriteFile(manifest, data, 0600))
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"cache", "tool-generation", "--manifest", manifest, "--cache-server-path", store, "--max-bytes", "100", "--apply"})
		require.NoError(t, root.Execute())
		var report struct {
			ID          string `json:"id"`
			Destination string `json:"destination"`
			Published   bool   `json:"published"`
			Reused      bool   `json:"reused"`
			Partial     bool   `json:"partial"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		require.True(t, report.Published)
		require.False(t, report.Partial)
		return report.ID, report.Destination, report.Reused
	}
	first, firstPath, reused := publish([]install{{"Go/1/x64", objects[0].ID}})
	require.False(t, reused)
	again, _, reused := publish([]install{{"Go/1/x64", objects[0].ID}})
	require.Equal(t, first, again)
	require.True(t, reused)
	second, secondPath, reused := publish([]install{{"Node/2/x64", objects[1].ID}, {"Go/1/x64", objects[0].ID}})
	require.False(t, reused)
	require.NotEqual(t, first, second)
	reordered, _, reused := publish([]install{{"Go/1/x64", objects[0].ID}, {"Node/2/x64", objects[1].ID}})
	require.Equal(t, second, reordered)
	require.True(t, reused, "input ordering must not produce another generation")
	original, err := os.Stat(filepath.Join(objects[0].Destination, "tree", "tool"))
	require.NoError(t, err)
	for _, generation := range []string{firstPath, secondPath} {
		reusedFile, err := os.Stat(filepath.Join(generation, "tree", "Go", "1", "x64", "tool"))
		require.NoError(t, err)
		require.True(t, os.SameFile(original, reusedFile), "successor must share closed data instead of copying it")
		marker, err := os.Stat(filepath.Join(generation, "tree", "Go", "1", "x64.complete"))
		require.NoError(t, err)
		require.True(t, marker.Mode().IsRegular())
	}
	_, err = os.Stat(filepath.Join(firstPath, "tree", "Node"))
	require.True(t, os.IsNotExist(err), "successor assembly must preserve the old generation")
}
