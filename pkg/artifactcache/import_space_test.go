package artifactcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportHeadroomRefusesInsufficientOrUnknownSpace(t *testing.T) {
	caches := []*Cache{{Size: 80}, {Size: 80}}
	for _, probe := range []importSpaceProbe{
		func(string) (uint64, error) { return 100, nil },
		func(string) (uint64, error) { return 0, errors.New("unreadable filesystem") },
	} {
		report := ImportReport{}
		require.Error(t, checkImportHeadroom("stage", caches, &report, probe))
		require.NotNil(t, report.RequiredAdditionalBytes)
		require.False(t, report.Published)
	}
}

func TestImportHeadroomAccountsAdditionalCopyWithoutChargingRetainedSourceTwice(t *testing.T) {
	retained := int64(1 << 40)
	report := ImportReport{RetainedSourceArchiveBytes: &retained}
	require.NoError(t, checkImportHeadroom("stage", []*Cache{{Size: 80}}, &report, func(string) (uint64, error) { return 1 << 30, nil }))
	require.NotNil(t, report.AvailableDestinationBytes)
	require.NotNil(t, report.RequiredAdditionalBytes)
	require.Less(t, *report.RequiredAdditionalBytes, uint64(1<<30))
	require.Equal(t, retained, *report.RetainedSourceArchiveBytes)
}

func TestImportHeadroomFailureCleansStageWithoutCopyingOrPublishing(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	before, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	db, err := openAuditDB(source.dir)
	require.NoError(t, err)
	defer db.Close()
	inventory := auditLocked(context.Background(), source, db, 0)
	cache := &Cache{}
	require.NoError(t, db.Get(uint64(1), cache))
	root := t.TempDir()
	report := ImportReport{Source: source.dir, Destination: filepath.Join(root, "repo")}
	err = publishImportWithSpace(context.Background(), source, db, inventory.Fingerprint, root, 80, []*Cache{cache}, &report, func(string) (uint64, error) { return 0, nil })
	require.ErrorContains(t, err, "insufficient destination headroom")
	require.False(t, report.Published)
	require.Zero(t, report.ImportedCount)
	require.Empty(t, report.PendingStage)
	require.Empty(t, report.Receipts)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "transfers.bolt", entries[0].Name())
	after, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	archive, err := os.ReadFile(source.storage.filename(1))
	require.NoError(t, err)
	require.Len(t, archive, 80)
}
