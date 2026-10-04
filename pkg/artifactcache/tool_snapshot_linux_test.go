//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func completedToolFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "install")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("warm"), 0600))
	require.NoError(t, os.Chmod(filepath.Join(source, "tool"), 0755))
	require.NoError(t, os.Symlink("tool", filepath.Join(source, "alias")))
	return source, filepath.Join(base, "objects")
}

func TestToolSnapshotPreservesExecutableAndRelativeSymlink(t *testing.T) {
	source, store := completedToolFixture(t)
	report := PublishToolSnapshot(context.Background(), source, store, 100)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	require.Equal(t, "sibling-v1", report.Completion)
	info, err := os.Stat(filepath.Join(report.Destination, "tree", "tool"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())
	target, err := os.Readlink(filepath.Join(report.Destination, "tree", "alias"))
	require.NoError(t, err)
	require.Equal(t, "tool", target)
}

func TestToolSnapshotRejectsIncompleteOverBudgetAndCancelledSources(t *testing.T) {
	for _, scenario := range []string{"incomplete", "budget", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			source, store := completedToolFixture(t)
			ctx := context.Background()
			limit := int64(100)
			switch scenario {
			case "incomplete":
				require.NoError(t, os.Rename(source+".complete", source+".unfinished"))
			case "budget":
				limit = 3
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			report := PublishToolSnapshot(ctx, source, store, limit)
			require.True(t, report.Partial)
			require.False(t, report.Published)
			_, err := os.Stat(store)
			require.True(t, os.IsNotExist(err), "rejected source must not initialize the store")
		})
	}
}

func TestToolSnapshotRefusesCorruptObjectWithoutReplacement(t *testing.T) {
	source, store := completedToolFixture(t)
	first := PublishToolSnapshot(context.Background(), source, store, 100)
	require.False(t, first.Partial, first.Error)
	payload := filepath.Join(first.Destination, "tree", "tool")
	require.NoError(t, os.WriteFile(payload, []byte("lost"), 0600))
	retry := PublishToolSnapshot(context.Background(), source, store, 100)
	require.True(t, retry.Partial)
	require.False(t, retry.Published)
	require.Equal(t, first.ID, retry.ID)
	actual, err := os.ReadFile(payload)
	require.NoError(t, err)
	require.Equal(t, "lost", string(actual), "publisher must preserve unexpected existing objects")
	entries, err := os.ReadDir(store)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tool-stage-")
	}
}

func TestToolSnapshotRefusesUnknownStore(t *testing.T) {
	source, store := completedToolFixture(t)
	require.NoError(t, os.Mkdir(store, 0755))
	foreign := filepath.Join(store, "unowned")
	require.NoError(t, os.WriteFile(foreign, []byte("keep"), 0600))
	report := PublishToolSnapshot(context.Background(), source, store, 100)
	require.True(t, report.Partial)
	require.False(t, report.Published)
	entries, err := os.ReadDir(store)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "unowned", entries[0].Name())
}

func TestToolSnapshotRejectsLinksToMutableExternalData(t *testing.T) {
	for _, kind := range []string{"absolute", "parent", "transitive"} {
		t.Run(kind, func(t *testing.T) {
			source, store := completedToolFixture(t)
			target := filepath.Join(source, "tool")
			if kind == "parent" {
				target = "../install/tool"
			}
			if kind == "transitive" {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(source), "outside"), []byte("mutable"), 0600))
				require.NoError(t, os.Symlink(".", filepath.Join(source, "nested")))
				target = "nested/../outside"
			}
			require.NoError(t, os.Symlink(target, filepath.Join(source, "external")))
			report := PublishToolSnapshot(context.Background(), source, store, 100)
			require.True(t, report.Partial, "a closed snapshot must not retain mutable external links")
			require.False(t, report.Published)
		})
	}
}

func TestToolSnapshotRejectsExcessiveDirectoryDepth(t *testing.T) {
	source, store := completedToolFixture(t)
	path := source
	for i := 0; i < 65; i++ {
		path = filepath.Join(path, "d")
		require.NoError(t, os.Mkdir(path, 0755))
	}
	report := PublishToolSnapshot(context.Background(), source, store, 100)
	require.True(t, report.Partial)
	require.Contains(t, report.Error, "depth limit")
	require.False(t, report.Published)
	_, err := os.Stat(store)
	require.True(t, os.IsNotExist(err))
}
