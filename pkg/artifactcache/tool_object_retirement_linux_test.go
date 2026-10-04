//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolObjectRetirementRequiresNoRetainedGenerationReferences(t *testing.T) {
	ctx := context.Background()
	source, store := completedToolFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("x"), 1<<20), 0600))
	old := PublishToolSnapshot(ctx, source, store, 10<<20)
	require.False(t, old.Partial, old.Error)
	first := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: old.ID}}}, 10<<20)
	require.False(t, first.Partial, first.Error)
	require.Error(t, RetireToolObject(ctx, store, old.ID, 10<<20, 100))
	reader, err := AcquireToolGenerationLease(ctx, store, first.Generation.ID, 10<<20)
	require.NoError(t, err)
	defer reader.Close()
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("y"), 1<<20), 0600))
	replacement := PublishToolSnapshot(ctx, source, store, 10<<20)
	require.False(t, replacement.Partial, replacement.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: replacement.ID}}}, 10<<20)
	require.False(t, next.Partial, next.Error)
	require.Error(t, RetireToolObject(ctx, store, old.ID, 10<<20, 100), "old living generation still refers to object")
	require.NoError(t, reader.Close())
	require.Error(t, RetireToolObject(ctx, store, old.ID, 10<<20, 100), "even an idle retained generation keeps object warm")
	require.NoError(t, RetireToolGeneration(ctx, store, first.Generation.ID, 10<<20))
	before := AuditToolStoreUsage(ctx, store, 10000)
	require.False(t, before.Partial, before.Error)
	require.NoError(t, RetireToolObject(ctx, store, old.ID, 10<<20, 100))
	after := AuditToolStoreUsage(ctx, store, 10000)
	require.False(t, after.Partial, after.Error)
	require.GreaterOrEqual(t, *before.AllocatedBytes-*after.AllocatedBytes, int64(1<<20))
	_, err = os.Lstat(old.Destination)
	require.True(t, os.IsNotExist(err))
	current, err := CurrentToolGeneration(ctx, store, 10<<20)
	require.NoError(t, err)
	require.Equal(t, next.Generation.ID, current.ID)
}

func TestToolObjectRetirementRefusesIncompleteReferenceInventory(t *testing.T) {
	for _, scenario := range []string{"unknown", "missing-manifest", "bound"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			source, store := completedToolFixture(t)
			warm := PublishToolSnapshot(ctx, source, store, 100)
			require.False(t, warm.Partial, warm.Error)
			first := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: warm.ID}}}, 100)
			require.False(t, first.Partial, first.Error)
			require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("orphan"), 0600))
			orphan := PublishToolSnapshot(ctx, source, store, 100)
			require.False(t, orphan.Partial, orphan.Error)
			require.NotEqual(t, warm.ID, orphan.ID)
			bound := 100
			switch scenario {
			case "unknown":
				require.NoError(t, os.Mkdir(filepath.Join(store, toolGenerationDirectory, "unknown"), 0700))
			case "missing-manifest":
				require.NoError(t, os.Rename(filepath.Join(first.Generation.Destination, "manifest.json"), filepath.Join(first.Generation.Destination, "manifest.missing")))
			case "bound":
				next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: warm.ID}}}, 100)
				require.False(t, next.Partial, next.Error)
				bound = 1
			}
			require.Error(t, RetireToolObject(ctx, store, orphan.ID, 100, bound))
			_, err := loadToolGenerationObject(ctx, orphan.Destination, orphan.ID, 100)
			require.NoError(t, err, "unreferenced payload must survive incomplete reference evidence")
		})
	}
}
