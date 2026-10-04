//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

func TestToolGenerationRetirementMountIdentityRefusal(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := InitializeToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	payload := filepath.Join(generation.Generation.Destination, "tree")
	err := verifyToolRetirementMounts(context.Background(), store, generation.Generation.Destination, func(path string) (uint64, error) {
		if path == payload {
			return 2, nil
		}
		return 1, nil
	})
	require.ErrorContains(t, err, "mount boundary")
	_, err = os.Stat(payload)
	require.NoError(t, err)
}

func TestToolGenerationRetirementActualBindMount(t *testing.T) {
	if os.Getenv("BOSN_TOOL_RETIREMENT_MOUNT_TEST") != "1" {
		t.Skip("requires private mount-capable container")
	}
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	source := filepath.Join(store, spec.Installs[0].ObjectID, "tree")
	target := filepath.Join(first.Generation.Destination, "tree", spec.Installs[0].Path)
	before, err := os.ReadDir(source)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	require.NoError(t, unix.Mount(source, target, "", unix.MS_BIND, ""))
	defer func() { require.NoError(t, unix.Unmount(target, 0)) }()
	var sourceStat, targetStat unix.Stat_t
	require.NoError(t, unix.Stat(source, &sourceStat))
	require.NoError(t, unix.Stat(target, &targetStat))
	require.Equal(t, sourceStat.Dev, targetStat.Dev, "regression must exercise same-device bind mount")
	require.ErrorContains(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100), "mount boundary")
	after, err := os.ReadDir(source)
	require.NoError(t, err)
	require.Len(t, after, len(before))
	_, err = loadToolGenerationObject(ctx, filepath.Join(store, spec.Installs[0].ObjectID), spec.Installs[0].ObjectID, 100)
	require.NoError(t, err, "mounted shared object payload must remain fully valid")
}
