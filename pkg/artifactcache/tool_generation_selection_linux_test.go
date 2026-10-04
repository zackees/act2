//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolGenerationUpdatesMergeConcurrentWarmInstalls(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	require.True(t, first.Selected)
	reader, err := AcquireToolGenerationLease(context.Background(), store, first.Generation.ID, 100)
	require.NoError(t, err)
	defer reader.Close()
	var results [2]ToolGenerationUpdateReport
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i, path := range []string{"Node/2/x64", "Python/3/x64"} {
		workers.Add(1)
		go func(index int, installPath string) {
			defer workers.Done()
			<-start
			update := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: installPath, ObjectID: spec.Installs[0].ObjectID}}}
			results[index] = UpdateToolGeneration(context.Background(), store, update, 100)
		}(i, path)
	}
	close(start)
	workers.Wait()
	for _, result := range results {
		require.False(t, result.Partial, result.Error)
		require.True(t, result.Selected)
	}
	current, err := CurrentToolGeneration(context.Background(), store, 100)
	require.NoError(t, err)
	manifest, _, err := readToolGenerationManifest(filepath.Join(store, toolGenerationDirectory, current.ID), current.ID)
	require.NoError(t, err)
	require.Len(t, manifest.Installs, 3, "a stale engine's updates must not discard another engine's warm installs")
	_, err = os.Stat(filepath.Join(first.Generation.Destination, "tree", "Node"))
	require.True(t, os.IsNotExist(err), "selected successor must not modify a living prior generation")
	repeat := UpdateToolGeneration(context.Background(), store, spec, 100)
	require.False(t, repeat.Partial, repeat.Error)
	require.True(t, repeat.Selected)
	require.True(t, repeat.Generation.Reused)
	require.Equal(t, current.ID, repeat.Generation.ID)
}

func TestToolGenerationSelectionRefusesUnknownCoordination(t *testing.T) {
	for _, scenario := range []string{"symlink", "oversized", "unknown-field", "missing-reader", "missing-generation"} {
		t.Run(scenario, func(t *testing.T) {
			store, spec := toolGenerationFixture(t)
			first := InitializeToolGeneration(context.Background(), store, spec, 100)
			require.False(t, first.Partial, first.Error)
			pointer := filepath.Join(store, toolGenerationCurrent)
			switch scenario {
			case "symlink":
				require.NoError(t, os.Rename(pointer, pointer+".original"))
				require.NoError(t, os.Symlink(pointer+".original", pointer))
			case "oversized":
				require.NoError(t, os.WriteFile(pointer, []byte(strings.Repeat("x", 513)), 0600))
			case "unknown-field":
				require.NoError(t, os.WriteFile(pointer, []byte(fmt.Sprintf(`{"schema_version":1,"id":%q,"extra":true}`, first.Generation.ID)), 0600))
			case "missing-reader":
				lock := filepath.Join(first.Generation.Destination, toolGenerationReaderLock)
				require.NoError(t, os.Rename(lock, lock+".missing"))
			case "missing-generation":
				require.NoError(t, os.Rename(first.Generation.Destination, first.Generation.Destination+".missing"))
			}
			before, err := os.ReadFile(pointer)
			require.NoError(t, err)
			_, err = CurrentToolGeneration(context.Background(), store, 100)
			require.Error(t, err)
			report := UpdateToolGeneration(context.Background(), store, spec, 100)
			require.True(t, report.Partial)
			require.False(t, report.Selected)
			after, err := os.ReadFile(pointer)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestToolGenerationSelectionReplaysLostDurabilityAcknowledgement(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := updateToolGenerationWithSelectionSync(context.Background(), store, spec, 100, true, func(string) error {
		return fmt.Errorf("injected selection directory sync failure after rename")
	})
	require.True(t, first.Partial)
	require.True(t, first.Selected, "visible selection must be reported even if final durability acknowledgement fails")
	require.True(t, first.Generation.Published)
	require.False(t, first.Generation.Partial)
	current, err := CurrentToolGeneration(context.Background(), store, 100)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, current.ID)
	retry := UpdateToolGeneration(context.Background(), store, spec, 100)
	require.False(t, retry.Partial, retry.Error)
	require.True(t, retry.Selected)
	require.True(t, retry.Generation.Reused)
	require.Equal(t, first.Generation.ID, retry.Generation.ID)
}

func TestToolGenerationUpdateRefusalPreservesWarmSelection(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	pointer := filepath.Join(store, toolGenerationCurrent)
	before, err := os.ReadFile(pointer)
	require.NoError(t, err)
	update := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}
	report := UpdateToolGeneration(context.Background(), store, update, 4)
	require.True(t, report.Partial)
	require.False(t, report.Selected)
	after, err := os.ReadFile(pointer)
	require.NoError(t, err)
	require.Equal(t, before, after)
	current, err := CurrentToolGeneration(context.Background(), store, 100)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, current.ID)
	bad := []byte("{\"schema_version\":2,\"id\":\"broken\"}")
	require.NoError(t, os.WriteFile(pointer, bad, 0600))
	report = UpdateToolGeneration(context.Background(), store, update, 100)
	require.True(t, report.Partial)
	require.False(t, report.Selected)
	after, err = os.ReadFile(pointer)
	require.NoError(t, err)
	require.Equal(t, bad, after, "malformed coordination must never be silently replaced")
}

func TestToolGenerationUpdateDoesNotBootstrapAfterLostSelection(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	pointer := filepath.Join(store, toolGenerationCurrent)
	require.NoError(t, os.Rename(pointer, pointer+".unavailable"))
	update := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}
	report := UpdateToolGeneration(context.Background(), store, update, 100)
	require.True(t, report.Partial, "a lost selection must refuse updates rather than discard prior warm installs")
	require.False(t, report.Selected)
	_, err := os.Lstat(pointer)
	require.True(t, os.IsNotExist(err))
}
