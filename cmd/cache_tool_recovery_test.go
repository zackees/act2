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
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
)

func TestToolRecoveryCommandRoundTripAndIntentBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux recovery references")
	}
	ctx := context.Background()
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "store")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), []byte("warm"), 0600))
	object := artifactcache.PublishToolSnapshot(ctx, source, store, 100)
	require.False(t, object.Partial, object.Error)
	generation := artifactcache.InitializeToolGeneration(ctx, store, artifactcache.ToolGenerationSpec{SchemaVersion: 1, Installs: []artifactcache.ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}}}, 100)
	require.False(t, generation.Partial, generation.Error)
	now := time.Now().UTC()
	pin := artifactcache.ToolRecoveryPin{SchemaVersion: 1, Owner: strings.Repeat("a", 64), Generation: generation.Generation.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	data, err := json.Marshal(pin)
	require.NoError(t, err)
	record := filepath.Join(base, "intent.json")
	require.NoError(t, os.WriteFile(record, data, 0600))
	run := func(operation string, extra ...string) ([]byte, error) {
		root := createRootCommand(ctx, &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		args := []string{"cache", "tool-recovery", operation, "--cache-server-path", store, "--record", record}
		root.SetArgs(append(args, extra...))
		err := root.Execute()
		return out.Bytes(), err
	}
	_, err = run("reserve", "--max-bytes", "100")
	require.ErrorContains(t, err, "--apply")
	out, err := run("reserve", "--max-bytes", "100", "--apply")
	require.NoError(t, err)
	var publication artifactcache.ToolRecoveryPinReport
	require.NoError(t, json.Unmarshal(out, &publication))
	require.True(t, publication.Published)
	out, err = run("release", "--apply")
	require.NoError(t, err)
	var release artifactcache.ToolRecoveryPinReleaseReport
	require.NoError(t, json.Unmarshal(out, &release))
	require.True(t, release.Absent)
	require.True(t, release.Removed)
	require.NoError(t, os.WriteFile(record, append(data, []byte(" {}")...), 0600))
	_, err = run("reserve", "--max-bytes", "100", "--apply")
	require.ErrorContains(t, err, "trailing")
	require.NoError(t, os.WriteFile(record, []byte(`{"unknown":1}`), 0600))
	_, err = run("release", "--apply")
	require.ErrorContains(t, err, "unknown field")
}
