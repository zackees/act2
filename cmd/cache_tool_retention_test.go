package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
)

func TestToolRetentionCommandRequiresApplyAndReportsProtectedOverflow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux coordinated tool retention")
	}
	ctx := context.Background()
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), []byte("warm"), 0600))
	object := artifactcache.PublishToolSnapshot(ctx, source, store, 100)
	require.False(t, object.Partial, object.Error)
	spec := artifactcache.ToolGenerationSpec{SchemaVersion: 1, Installs: []artifactcache.ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}}}
	first := artifactcache.InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := artifactcache.UpdateToolGeneration(ctx, store, artifactcache.ToolGenerationSpec{SchemaVersion: 1, Installs: []artifactcache.ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: object.ID}}}, 100)
	require.False(t, next.Partial, next.Error)
	args := []string{"cache", "tool-retain", "--cache-server-path", store, "--max-allocated-bytes", "1", "--expire-before", time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "--max-entries", "10000", "--max-candidates", "100", "--max-payload-bytes", "100"}
	root := createRootCommand(ctx, &Input{}, "test")
	root.SetArgs(args)
	require.ErrorContains(t, root.Execute(), "--apply")
	_, err := os.Stat(first.Generation.Destination)
	require.NoError(t, err, "command refusal must precede deletion")
	root = createRootCommand(ctx, &Input{}, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(append(args, "--apply"))
	require.ErrorContains(t, root.Execute(), "overflow")
	var report artifactcache.ToolRetentionReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.False(t, report.Partial, report.Error)
	require.True(t, report.ProtectedOverflow)
	require.Contains(t, report.RetiredGenerations, first.Generation.ID)
	require.NotNil(t, report.After.AllocatedBytes)
	selected, err := artifactcache.CurrentToolGeneration(ctx, store, 100)
	require.NoError(t, err)
	require.Equal(t, next.Generation.ID, selected.ID)
}
