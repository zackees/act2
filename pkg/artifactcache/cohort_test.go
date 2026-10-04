package artifactcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCohortExcludesMaintenanceWhileServerLives(t *testing.T) {
	root := t.TempDir()
	policy := DefaultPolicy()
	policy.CohortRoot = root
	dir := filepath.Join(root, "repo")
	h, err := StartHandlerWithPolicy(dir, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	gate := &Handler{dir: root}
	lock, err := gate.transferLock(false)
	require.Error(t, err, "an active server must exclude aggregate maintenance")
	require.Nil(t, lock)
	require.NoError(t, h.Close())
	lock, err = gate.transferLock(false)
	require.NoError(t, err)
	defer lock.Close()
	lease, err := enrollCohort(filepath.Join(root, "new-repo"), root)
	require.Error(t, err, "new namespace creation must wait for aggregate maintenance")
	require.Nil(t, lease)
	_, err = os.Stat(filepath.Join(root, "new-repo"))
	require.True(t, os.IsNotExist(err))
}

func TestCohortRejectsLegacyNamespaceAndImplicitAccess(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	seedAuditStore(t, legacy, 2)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	h, err := StartHandlerWithPolicy(legacy, "", "127.0.0.1", 0, nil, policy)
	require.ErrorContains(t, err, "legacy")
	require.Nil(t, h)
	require.EqualValues(t, 160, *AuditStore(t.Context(), legacy, 0).ArchiveBytes)
	modern := filepath.Join(root, "modern")
	h, err = StartHandlerWithPolicy(modern, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	h, err = StartHandler(modern, "", "127.0.0.1", 0, nil)
	require.ErrorContains(t, err, "cohort root")
	require.Nil(t, h)
}
