package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolSnapshotPublicationPreservesOldPayloadAndReusesAnUnchangedInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("immutable tool publication is for Linux Docker engines")
	}
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".complete"), nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("warm"), 0600))
	require.NoError(t, os.Chmod(filepath.Join(source, "tool"), 0755))
	publish := func() (string, string, bool) {
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"cache", "tool-publish", "--from", source, "--cache-server-path", store,
			"--max-bytes", "100", "--apply", "--source-quiescent"})
		require.NoError(t, root.Execute())
		var report struct {
			SchemaVersion int    `json:"schema_version"`
			ID            string `json:"id"`
			Destination   string `json:"destination"`
			Published     bool   `json:"published"`
			Reused        bool   `json:"reused"`
			Partial       bool   `json:"partial"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		require.Equal(t, 1, report.SchemaVersion)
		require.True(t, report.Published)
		require.False(t, report.Partial)
		require.Len(t, report.ID, 64)
		require.Equal(t, filepath.Join(store, report.ID), report.Destination)
		return report.ID, report.Destination, report.Reused
	}
	first, destination, reused := publish()
	require.False(t, reused)
	second, _, reused := publish()
	require.Equal(t, first, second)
	require.True(t, reused)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("new!"), 0600))
	third, _, reused := publish()
	require.NotEqual(t, first, third)
	require.False(t, reused)
	closed, err := os.ReadFile(filepath.Join(destination, "tree", "tool"))
	require.NoError(t, err)
	require.Equal(t, "warm", string(closed))
	entries, err := os.ReadDir(store)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".stage-")
	}
}
