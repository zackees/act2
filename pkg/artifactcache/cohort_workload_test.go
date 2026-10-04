package artifactcache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func assertArchiveHit(t *testing.T, h *Handler, key string) {
	t.Helper()
	lookup := httptest.NewRecorder()
	h.router.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/"+h.token+apiPath+"/cache?keys="+key+"&version=v", nil))
	require.Equal(t, http.StatusOK, lookup.Code, lookup.Body.String())
	var hit struct {
		Location string `json:"archiveLocation"`
		Key      string `json:"cacheKey"`
	}
	require.NoError(t, json.Unmarshal(lookup.Body.Bytes(), &hit))
	require.Equal(t, key, hit.Key)
	archive := httptest.NewRecorder()
	h.router.ServeHTTP(archive, httptest.NewRequest(http.MethodGet, hit.Location, nil))
	require.Equal(t, http.StatusOK, archive.Code)
	require.Equal(t, make([]byte, 80), archive.Body.Bytes())
}

func TestRepeatedFreshServersKeepImportedHitsAndBoundIdleCohort(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	root := filepath.Join(t.TempDir(), "cohort")
	imported := ImportCompleted(context.Background(), source.dir, root, "warm", 80)
	require.False(t, imported.Partial, imported.Error)
	require.True(t, imported.Published)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	policy.MaxBytes = 160
	for cycle := 0; cycle < 8; cycle++ {
		seedCohortStore(t, root, fmt.Sprintf("cold-%d", cycle), 2)
		h, err := StartHandlerWithPolicy(imported.Destination, "", "127.0.0.1", 0, nil, policy)
		require.NoError(t, err)
		assertArchiveHit(t, h, "warm-0")
		busy := MaintainCohort(context.Background(), root, 160, policy)
		require.True(t, busy.Partial, "active cohort must defer")
		require.Nil(t, busy.BudgetMet)
		require.NoError(t, h.Close())
		idle := MaintainCohort(context.Background(), root, 160, policy)
		require.False(t, idle.Partial, idle.Errors)
		require.True(t, *idle.BudgetMet)
		require.LessOrEqual(t, *idle.RemainingCompletedBytes, int64(160))
		warm := AuditStore(context.Background(), imported.Destination, 0)
		require.False(t, warm.Partial, warm.Errors)
		require.EqualValues(t, 80, *warm.ArchiveBytes)
	}
	h, err := StartHandlerWithPolicy(imported.Destination, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	assertArchiveHit(t, h, "warm-0")
}
