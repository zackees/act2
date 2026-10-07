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

func TestToolGenerationUpdateWaitsForCatalogWithinOperationBudget(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan ToolGenerationUpdateReport, 1)
	go func() { finished <- UpdateToolGeneration(ctx, store, spec, 100) }()
	select {
	case report := <-finished:
		t.Fatalf("ordinary update rejected legitimate catalog contention: %+v", report)
	case <-time.After(250 * time.Millisecond):
	}
	require.NoError(t, catalog.Close())
	select {
	case report := <-finished:
		require.False(t, report.Partial, report.Error)
		require.True(t, report.Selected)
		require.Equal(t, first.Generation.ID, report.Generation.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("update did not resume after original catalog lease closed")
	}
}

func TestToolGenerationUpdateCatalogWaitHonorsCancellation(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	defer catalog.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan ToolGenerationUpdateReport, 1)
	go func() { finished <- UpdateToolGeneration(ctx, store, spec, 100) }()
	// Keep the original catalog inode locked beyond its exclusive-probe bound.
	time.Sleep(250 * time.Millisecond)
	cancel()
	select {
	case report := <-finished:
		require.True(t, report.Partial)
		require.False(t, report.Selected)
		require.Equal(t, context.Canceled.Error(), report.Error)
	case <-time.After(time.Second):
		t.Fatal("cancelled update kept waiting for catalog")
	}
	selection, err := readToolGenerationSelection(store)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, selection.ID)
	_, _, err = readToolGenerationManifest(filepath.Join(store, toolGenerationDirectory, selection.ID), selection.ID)
	require.NoError(t, err)
}

func TestToolGenerationUpdateCatalogWaitHonorsDeadline(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	defer catalog.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	report := UpdateToolGeneration(ctx, store, spec, 100)
	require.True(t, report.Partial)
	require.False(t, report.Selected)
	require.Equal(t, context.DeadlineExceeded.Error(), report.Error)
}

func TestToolGenerationUpdateNeverRecreatesMissingCatalog(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	path := filepath.Join(store, toolStoreLock)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	defer catalog.Close()
	require.NoError(t, os.Rename(path, path+".held"))
	report := UpdateToolGeneration(context.Background(), store, spec, 100)
	require.True(t, report.Partial)
	require.False(t, report.Selected)
	_, err = os.Lstat(path)
	require.True(t, os.IsNotExist(err), "ordinary updates cannot replace a living catalog inode")
	selection, err := readToolGenerationSelection(store)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, selection.ID)
}
