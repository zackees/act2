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

// Harness runs prepare with cap-drop ALL and recover in a new helper with
// DAC_OVERRIDE, mounting the same private Docker volume at /fixture.
func TestToolRetirementAcrossHelperMountNamespaces(t *testing.T) {
	phase := os.Getenv("ACT_TEST_RETIREMENT_HANDOFF")
	if phase == "" {
		t.Skip("requires two isolated helpers sharing a private fixture volume")
	}
	ctx := context.Background()
	store := "/fixture/objects"
	switch phase {
	case "prepare":
		source := "/fixture/source"
		require.NoError(t, os.Mkdir(source, 0755))
		require.NoError(t, os.WriteFile(source+".complete", nil, 0600))
		require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), []byte("warm"), 0600))
		warm := PublishToolSnapshot(ctx, source, store, 10000)
		require.False(t, warm.Partial, warm.Error)
		selected := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: warm.ID}}}, 10000)
		require.False(t, selected.Partial, selected.Error)
		require.NoError(t, os.WriteFile("/fixture/selected", []byte(selected.Generation.ID), 0600))
		directory := filepath.Join(source, "readonly")
		require.NoError(t, os.Mkdir(directory, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "payload"), []byte("orphan"), 0600))
		require.NoError(t, os.Chmod(directory, 0555))
		orphan := PublishToolSnapshot(ctx, source, store, 10000)
		require.False(t, orphan.Partial, orphan.Error)
		require.ErrorContains(t, RetireToolObject(ctx, store, orphan.ID, 10000, 100), "stage retirement incomplete")
		catalog, err := prepareExistingToolSnapshotStore(store)
		require.NoError(t, err)
		defer catalog.Close()
		rows, err := loadToolStageOwnership(catalog, store)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		t.Logf("original helper mount=%d root=%d:%d stage=%d:%d", rows[0].MountID, rows[0].RootDevice, rows[0].RootInode, rows[0].Device, rows[0].Inode)
	case "recover":
		catalog, err := prepareExistingToolSnapshotStore(store)
		require.NoError(t, err)
		rows, err := loadToolStageOwnership(catalog, store)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		mount, err := toolMountID(store)
		require.NoError(t, err)
		require.NotEqual(t, rows[0].MountID, mount, "fixture must use distinct helper mount namespaces")
		t.Logf("replacement helper mount=%d original mount=%d", mount, rows[0].MountID)
		stage := filepath.Join(store, rows[0].Relative)
		require.NoError(t, catalog.Close())
		_, err = os.Lstat(filepath.Join(stage, "manifest.json"))
		require.True(t, os.IsNotExist(err))
		report := RetireToolStages(ctx, store, time.Now().Add(time.Hour), 10000)
		require.False(t, report.Partial, report.Error)
		require.Contains(t, report.RetiredStages, stage)
		_, err = os.Lstat(stage)
		require.True(t, os.IsNotExist(err))
		expected, err := os.ReadFile("/fixture/selected")
		require.NoError(t, err)
		current, err := CurrentToolGeneration(ctx, store, 10000)
		require.NoError(t, err)
		require.Equal(t, string(expected), current.ID)
	default:
		t.Fatalf("invalid isolated handoff phase %q", phase)
	}
}
