//go:build linux

package artifactcache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolStageExpiryRequiresOriginalCatalogOwnership(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage, "incomplete"), []byte("owned"), 0600))
	unknown, err := os.MkdirTemp(store, ".tool-stage-")
	require.NoError(t, err)
	blocked := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.True(t, blocked.Partial, "living publisher's original catalog lock must prevent sweep")
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredStages, stage)
	_, err = os.Lstat(stage)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(unknown)
	require.NoError(t, err, "prefix alone does not prove ownership")
}

func TestToolStageExpiryPreservesReplacedDirectoryIdentity(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	require.NoError(t, os.Rename(stage, stage+".original"))
	require.NoError(t, os.Mkdir(stage, 0700))
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.True(t, report.Partial)
	_, err = os.Stat(stage)
	require.NoError(t, err)
	_, err = os.Stat(stage + ".original")
	require.NoError(t, err)
}

func TestToolStageExpiryClearsPostRenameRecordWithoutDeletingPublication(t *testing.T) {
	source, store := completedToolFixture(t)
	report := publishToolSnapshotWithSync(context.Background(), source, store, 100, func(string) error { return fmt.Errorf("lost post-rename sync acknowledgement") })
	require.True(t, report.Published)
	require.True(t, report.Partial)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Len(t, rows, 1, "uncertain publication retains durable stage intent")
	require.NoError(t, catalog.Close())
	swept := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.False(t, swept.Partial, swept.Error)
	require.Empty(t, swept.RetiredStages, "published payload is not a leftover stage")
	_, err = loadToolGenerationObject(context.Background(), report.Destination, report.ID, 100)
	require.NoError(t, err)
	catalog, err = prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	defer catalog.Close()
	rows, err = loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestToolStageOwnershipCapacityRefusesBeforeCreatingStage(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	directory := filepath.Join(store, toolStageOwnershipDirectory)
	for i := 1; i < 10000; i++ {
		row := rows[0]
		row.Relative = fmt.Sprintf(".tool-stage-reserved-%d", i)
		row.CreatedAt = time.Now().Add(24 * time.Hour).UTC()
		data, err := json.Marshal(row)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, toolStageRecordName(row.Relative)), data, 0600))
	}
	refused, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.ErrorContains(t, err, "capacity")
	require.Empty(t, refused, "capacity refusal must precede stage creation")
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 100000)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredStages, stage, "capacity refusal must preserve recovery for existing records")
}
