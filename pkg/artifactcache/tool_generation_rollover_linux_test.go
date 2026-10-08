//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolGenerationRolloverWithinSelectedPayloadBound(t *testing.T) {
	ctx := context.Background()
	source, store := completedToolFixture(t)
	old := PublishToolSnapshot(ctx, source, store, 4)
	require.False(t, old.Partial, old.Error)
	first := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: old.ID}}}, 4)
	require.False(t, first.Partial, first.Error)
	reader, err := AcquireToolGenerationLease(ctx, store, first.Generation.ID, 4)
	require.NoError(t, err)
	defer reader.Close()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("next"), 0600))
	fresh := PublishToolSnapshot(ctx, source, store, 4)
	require.False(t, fresh.Partial, fresh.Error)
	// Bosn needs an explicit exact-selection operation: implicit merging cannot
	// rotate versions when each selected generation must stay inside this bound.
	successor := ReplaceToolGeneration(ctx, store, first.Generation.ID, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/2/x64", ObjectID: fresh.ID}}}, 4)
	require.False(t, successor.Partial, successor.Error)
	require.True(t, successor.Selected)
	current, err := CurrentToolGeneration(ctx, store, 4)
	require.NoError(t, err)
	require.Equal(t, successor.Generation.ID, current.ID)
	payload, err := os.ReadFile(filepath.Join(first.Generation.Destination, "tree", "Go/1/x64/tool"))
	require.NoError(t, err)
	require.Equal(t, "warm", string(payload), "a selected successor must not alter an older reader's payload")
	held := RetainToolStore(ctx, store, ToolRetentionPolicy{MaxAllocatedBytes: 1 << 20, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 4})
	require.False(t, held.Partial, held.Error)
	require.Contains(t, held.ProtectedGenerations, first.Generation.ID)
	require.NoError(t, reader.Close())
	retired := RetainToolStore(ctx, store, ToolRetentionPolicy{MaxAllocatedBytes: 1 << 20, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 4})
	require.False(t, retired.Partial, retired.Error)
	require.Contains(t, retired.RetiredGenerations, first.Generation.ID)
	require.Contains(t, retired.RetiredObjects, old.ID)
}

func TestToolGenerationReplacementRefusesMissingOrOversizedSelection(t *testing.T) {
	for _, scenario := range []string{"missing", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, spec := toolGenerationFixture(t)
			var current ToolGenerationUpdateReport
			if scenario == "oversized" {
				current = InitializeToolGeneration(ctx, store, spec, 4)
				require.False(t, current.Partial, current.Error)
				spec.Installs = append(spec.Installs, ToolGenerationInstall{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID})
			}
			expected := current.Generation.ID
			if expected == "" {
				expected = strings.Repeat("0", 64)
			}
			replacement := ReplaceToolGeneration(ctx, store, expected, spec, 4)
			require.True(t, replacement.Partial)
			require.False(t, replacement.Selected)
			if scenario == "missing" {
				_, err := os.Lstat(filepath.Join(store, toolGenerationCurrent))
				require.True(t, os.IsNotExist(err), "replacement cannot bootstrap missing authority")
			} else {
				selected, err := CurrentToolGeneration(ctx, store, 4)
				require.NoError(t, err)
				require.Equal(t, current.Generation.ID, selected.ID)
			}
		})
	}
}

func TestToolGenerationReplacementRefusesStaleSuccessor(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 8)
	require.False(t, first.Partial, first.Error)
	concurrent := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 8)
	require.False(t, concurrent.Partial, concurrent.Error)
	replacement := ReplaceToolGeneration(ctx, store, first.Generation.ID, spec, 8)
	require.True(t, replacement.Partial)
	require.False(t, replacement.Selected)
	require.Contains(t, replacement.Error, "selection changed")
	current, err := CurrentToolGeneration(ctx, store, 8)
	require.NoError(t, err)
	require.Equal(t, concurrent.Generation.ID, current.ID)
}
