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

func TestToolRetentionPressureReclaimsUnreferencedObjectPayload(t *testing.T) {
	ctx := context.Background()
	source, store := completedToolFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("x"), 1<<20), 0600))
	old := PublishToolSnapshot(ctx, source, store, 10<<20)
	require.False(t, old.Partial, old.Error)
	first := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: old.ID}}}, 10<<20)
	require.False(t, first.Partial, first.Error)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("y"), 1<<20), 0600))
	replacement := PublishToolSnapshot(ctx, source, store, 10<<20)
	require.False(t, replacement.Partial, replacement.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: replacement.ID}}}, 10<<20)
	require.False(t, next.Partial, next.Error)
	report := RetainToolStore(ctx, store, ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: time.Now().Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 10 << 20})
	require.False(t, report.Partial, report.Error)
	_, err := os.Lstat(old.Destination)
	require.True(t, os.IsNotExist(err), "pressure sweep must retire unreferenced old payload, not only generation metadata")
	require.GreaterOrEqual(t, *report.Before.AllocatedBytes-*report.After.AllocatedBytes, int64(1<<20))
	_, err = loadToolGenerationObject(ctx, replacement.Destination, replacement.ID, 10<<20)
	require.NoError(t, err)
	require.True(t, report.ProtectedOverflow)
}
