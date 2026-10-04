//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolRetentionExpiresOldGenerationAndReportsProtectedOverflow(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	unknown := filepath.Join(store, "unknown-owned-by-someone-else")
	require.NoError(t, os.WriteFile(unknown, []byte("preserve"), 0600))
	policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
	report := RetainToolStore(ctx, store, policy)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredGenerations, first.Generation.ID)
	require.True(t, report.ProtectedOverflow)
	require.NotNil(t, report.After.AllocatedBytes)
	require.Greater(t, *report.After.AllocatedBytes, int64(1))
	_, err := os.Lstat(first.Generation.Destination)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(next.Generation.Destination)
	require.NoError(t, err)
	_, err = os.Stat(unknown)
	require.NoError(t, err)
}

func TestToolRetentionPreservesLivingOldReaderThenConverges(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	reader, err := AcquireToolGenerationLease(ctx, store, first.Generation.ID, 100)
	require.NoError(t, err)
	defer reader.Close()
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
	report := RetainToolStore(ctx, store, policy)
	require.Empty(t, report.RetiredGenerations)
	require.True(t, report.ProtectedOverflow)
	_, err = os.Stat(first.Generation.Destination)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	report = RetainToolStore(ctx, store, policy)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredGenerations, first.Generation.ID)
	current, err := CurrentToolGeneration(ctx, store, 100)
	require.NoError(t, err)
	require.Equal(t, next.Generation.ID, current.ID)
}

func TestToolRetentionPreservesYoungGenerationUntilPressure(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	policy := ToolRetentionPolicy{MaxAllocatedBytes: 100 << 20, ExpireBefore: time.Now().Add(-time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
	report := RetainToolStore(ctx, store, policy)
	require.False(t, report.Partial, report.Error)
	require.Empty(t, report.RetiredGenerations)
	require.False(t, report.ProtectedOverflow)
	_, err := os.Stat(first.Generation.Destination)
	require.NoError(t, err)
	policy.MaxAllocatedBytes = 1
	report = RetainToolStore(ctx, store, policy)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredGenerations, first.Generation.ID)
	require.True(t, report.ProtectedOverflow)
}

func TestToolRetentionRefusesIncompleteInventoryBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"entry-bound", "candidate-bound", "invalid-cap"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, spec := toolGenerationFixture(t)
			first := InitializeToolGeneration(ctx, store, spec, 100)
			require.False(t, first.Partial, first.Error)
			next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
			require.False(t, next.Partial, next.Error)
			policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
			switch scenario {
			case "entry-bound":
				policy.MaxEntries = 1
			case "candidate-bound":
				policy.MaxCandidates = 1
			case "invalid-cap":
				policy.MaxAllocatedBytes = 0
			}
			report := RetainToolStore(ctx, store, policy)
			require.True(t, report.Partial)
			require.Empty(t, report.RetiredGenerations)
			_, err := os.Stat(first.Generation.Destination)
			require.NoError(t, err)
		})
	}
}
