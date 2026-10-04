//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolRetentionCombinedSweepExpiresOwnedStage(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	current := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, current.Partial, current.Error)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage, "incomplete"), bytes.Repeat([]byte("x"), 1<<20), 0600))
	require.NoError(t, catalog.Close())
	report := RetainToolStore(context.Background(), store, ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100})
	require.False(t, report.Partial, report.Error)
	_, err = os.Lstat(stage)
	require.True(t, os.IsNotExist(err), "combined sweep must expire registered old stage")
	require.GreaterOrEqual(t, *report.Before.AllocatedBytes-*report.After.AllocatedBytes, int64(1<<20))
	selected, err := CurrentToolGeneration(context.Background(), store, 100)
	require.NoError(t, err)
	require.Equal(t, current.Generation.ID, selected.ID)
}
