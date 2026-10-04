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

func TestToolGenerationRetirementMeasuresReclamationWithoutCountingSharedPayload(t *testing.T) {
	ctx := context.Background()
	source, store := completedToolFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("x"), 1<<20), 0600))
	object := PublishToolSnapshot(ctx, source, store, 10<<20)
	require.False(t, object.Partial, object.Error)
	spec := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}}}
	first := InitializeToolGeneration(ctx, store, spec, 10<<20)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: object.ID}}}, 10<<20)
	require.False(t, next.Partial, next.Error)
	before := AuditToolStoreUsage(ctx, store, 10000)
	require.False(t, before.Partial, before.Error)
	require.NoError(t, RetireToolGeneration(ctx, store, first.Generation.ID, 10<<20))
	after := AuditToolStoreUsage(ctx, store, 10000)
	require.False(t, after.Partial, after.Error)
	reclaimed := *before.AllocatedBytes - *after.AllocatedBytes
	require.Greater(t, reclaimed, int64(0), "generation metadata allocation should be reclaimed")
	require.Less(t, reclaimed, int64(1<<20), "shared payload is still allocated despite removing its generation reference")
	require.GreaterOrEqual(t, *after.UniqueFileBytes, int64(1<<20))
	require.Equal(t, int64(2<<20), *after.ReferencedFileBytes-*after.UniqueFileBytes)
	t.Logf("allocated_before=%d allocated_after=%d reclaimed=%d payload_bytes=%d", *before.AllocatedBytes, *after.AllocatedBytes, reclaimed, 1<<20)
}
