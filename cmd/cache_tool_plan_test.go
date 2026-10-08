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

func TestToolSnapshotPlanDoesNotCreateStoreAndRejectsChangedSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux tool snapshots")
	}
	base := t.TempDir()
	source, store := filepath.Join(base, "install"), filepath.Join(base, "objects")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".complete"), nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("warm"), 0600))
	run := func(flags ...string) (artifactcache.ToolSnapshotReport, error) {
		command := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		command.SetOut(&out)
		args := []string{"cache", "tool-publish", "--from", source, "--cache-server-path", store, "--max-bytes", "4", "--source-quiescent"}
		command.SetArgs(append(args, flags...))
		err := command.Execute()
		var report artifactcache.ToolSnapshotReport
		if out.Len() > 0 {
			require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		}
		return report, err
	}
	plan, err := run("--plan")
	require.NoError(t, err)
	require.False(t, plan.Partial)
	require.False(t, plan.Published)
	require.False(t, plan.Reused)
	require.Len(t, plan.ID, 64)
	require.NotNil(t, plan.Bytes)
	require.EqualValues(t, 4, *plan.Bytes)
	_, err = os.Lstat(store)
	require.True(t, os.IsNotExist(err))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("next"), 0600))
	changed, err := run("--apply", "--expected-object", plan.ID)
	require.ErrorContains(t, err, "changed since planning")
	require.True(t, changed.Partial)
	require.False(t, changed.Published)
	_, err = os.Lstat(store)
	require.True(t, os.IsNotExist(err))
	invalid, err := run("--apply", "--expected-object", "")
	require.ErrorContains(t, err, "canonical digest")
	require.False(t, invalid.Published)
	_, err = os.Lstat(store)
	require.True(t, os.IsNotExist(err))
	next, err := run("--plan")
	require.NoError(t, err)
	require.NotEqual(t, plan.ID, next.ID)
	published, err := run("--apply", "--expected-object", next.ID)
	require.NoError(t, err)
	require.True(t, published.Published)
	require.Equal(t, next.ID, published.ID)
}
