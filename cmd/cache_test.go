package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nektos/act/pkg/artifactcache"
)

func TestOfflineAuditCommandDoesNotCreateAMissingStore(t *testing.T) {
	for _, args := range [][]string{{"cache", "audit"}, {"cache", "prune"}, {"cache", "prune", "--apply"}} {
		t.Run(args[1], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "missing")
			root := createRootCommand(context.Background(), &Input{}, "test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs(append(args, "--cache-server-path", dir))
			require.NoError(t, root.Execute())
			var report artifactcache.StoreAudit
			require.NoError(t, json.Unmarshal(out.Bytes(), &report))
			require.Equal(t, artifactcache.StoreMissing, report.Status)
			require.Nil(t, report.ArchiveBytes)
			_, err := os.Stat(dir)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestOfflineAuditFailsOnUnsupportedLegacyCoordination(t *testing.T) {
	dir := t.TempDir()
	root := createRootCommand(context.Background(), &Input{}, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"cache", "audit", "--cache-server-path", dir})
	require.Error(t, root.Execute())
	var report artifactcache.StoreAudit
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.True(t, report.Partial)
	require.Equal(t, artifactcache.StorePartial, report.Status)
	require.Nil(t, report.EntryCount)
	_, err := os.Stat(filepath.Join(dir, "transfers.bolt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOfflineCohortCommandRequiresApplyAndDoesNotCreateRoot(t *testing.T) {
	for _, apply := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "missing")
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		args := []string{"cache", "prune-cohort", "--max-bytes", "100", "--cache-server-path", dir}
		if apply {
			args = append(args, "--apply")
		}
		root.SetArgs(args)
		require.Error(t, root.Execute())
		if apply {
			var report artifactcache.CohortReport
			require.NoError(t, json.Unmarshal(out.Bytes(), &report))
			require.True(t, report.Partial)
			require.EqualValues(t, 100, report.BudgetBytes)
			require.Nil(t, report.BudgetMet)
		} else {
			require.Empty(t, out.String())
		}
		_, err := os.Stat(dir)
		require.True(t, os.IsNotExist(err))
	}
}
