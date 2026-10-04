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

func TestToolUsageCommandReportsCompleteAndUnknownTotals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux tool-store inode accounting")
	}
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), []byte("warm"), 0600))
	object := artifactcache.PublishToolSnapshot(context.Background(), source, store, 100)
	require.False(t, object.Partial, object.Error)
	for _, bound := range []string{"1000", "1"} {
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"cache", "tool-usage", "--cache-server-path", store, "--max-entries", bound})
		err := root.Execute()
		var report artifactcache.ToolStoreUsage
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		require.Equal(t, 1, report.SchemaVersion)
		require.Equal(t, store, report.Root)
		if bound == "1000" {
			require.NoError(t, err)
			require.False(t, report.Partial, report.Error)
			require.NotNil(t, report.AllocatedBytes)
			require.Greater(t, *report.AllocatedBytes, int64(0))
			require.Equal(t, report.UniqueFileBytes, report.ReferencedFileBytes)
		} else {
			require.Error(t, err)
			require.True(t, report.Partial)
			require.Nil(t, report.AllocatedBytes)
			require.Nil(t, report.ApparentBytes)
		}
	}
}
