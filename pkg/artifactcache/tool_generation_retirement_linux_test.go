//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolGenerationRetirementProtectsSelectionAndLiveReader(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	require.Error(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100))
	reader, err := AcquireToolGenerationLease(ctx, store, first.Generation.ID, 100)
	require.NoError(t, err)
	defer reader.Close()
	update := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}
	next := UpdateToolGeneration(ctx, store, update, 100)
	require.False(t, next.Partial, next.Error)
	require.NotEqual(t, first.Generation.ID, next.Generation.ID)
	require.Error(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100))
	_, err = os.Stat(first.Generation.Destination)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100))
	_, err = os.Lstat(first.Generation.Destination)
	require.True(t, os.IsNotExist(err))
	current, err := CurrentToolGeneration(ctx, store, 100)
	require.NoError(t, err)
	require.Equal(t, next.Generation.ID, current.ID)
	_, err = os.Stat(filepath.Join(store, spec.Installs[0].ObjectID, "tree"))
	require.NoError(t, err, "retiring generation must preserve shared immutable objects")
}

func TestToolGenerationRetirementRefusesLostSelection(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	require.NoError(t, os.Rename(filepath.Join(store, toolGenerationCurrent), filepath.Join(store, ".lost-selection")))
	require.Error(t, RetireToolGeneration(context.Background(), store, first.Generation.ID, 100))
	_, err := os.Stat(first.Generation.Destination)
	require.NoError(t, err)
}

func TestToolGenerationRetirementPreservesUnknownOrMissingCoordination(t *testing.T) {
	for _, scenario := range []string{"unknown", "missing-reader"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, spec := toolGenerationFixture(t)
			first := InitializeToolGeneration(ctx, store, spec, 100)
			require.False(t, first.Partial, first.Error)
			update := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}
			next := UpdateToolGeneration(ctx, store, update, 100)
			require.False(t, next.Partial, next.Error)
			if scenario == "unknown" {
				require.NoError(t, os.WriteFile(filepath.Join(first.Generation.Destination, "unknown"), []byte("preserve"), 0600))
			} else {
				lock := filepath.Join(first.Generation.Destination, toolGenerationReaderLock)
				require.NoError(t, os.Rename(lock, lock+".missing"))
			}
			require.Error(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100))
			_, err := os.Stat(first.Generation.Destination)
			require.NoError(t, err)
		})
	}
}
