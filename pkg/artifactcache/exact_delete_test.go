package artifactcache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
)

func TestExactDeletePreservesOtherKeysAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 2)
	request := httptest.NewRequest(http.MethodDelete, "/"+h.token+apiPath+"/cache?key=warm-0", nil)
	writer := httptest.NewRecorder()
	h.router.ServeHTTP(writer, request)
	require.Equal(t, http.StatusOK, writer.Code)
	var result struct {
		SchemaVersion int    `json:"schema_version"`
		Key           string `json:"key"`
		DeletedCount  uint64 `json:"deleted_count"`
		Bytes         int64  `json:"reclaimed_archive_bytes"`
	}
	require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &result))
	require.Equal(t, 1, result.SchemaVersion)
	require.Equal(t, "warm-0", result.Key)
	require.EqualValues(t, 1, result.DeletedCount)
	require.EqualValues(t, 80, result.Bytes)
	writer = httptest.NewRecorder()
	h.router.ServeHTTP(writer, request)
	require.Equal(t, http.StatusOK, writer.Code, "absent exact keys are idempotent")
	require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &result))
	require.Zero(t, result.DeletedCount)
	proof := AuditStore(context.Background(), dir, 0)
	require.False(t, proof.Partial, proof.Errors)
	require.Len(t, proof.Entries, 1)
	require.Equal(t, "warm-1", proof.Entries[0].Key)
	require.EqualValues(t, 80, *proof.ArchiveBytes)
}

func TestExactDeleteRejectsInvalidKeysAndActiveTransfers(t *testing.T) {
	h := seedAuditStore(t, t.TempDir(), 1)
	for _, query := range []string{"", "key=", "key=warm-0&key=warm-1", "key=warm-0&ref=main", "key=%00", "key=warm-0&%zz", "key=%7f", "key=" + strings.Repeat("x", 2048)} {
		writer := httptest.NewRecorder()
		h.router.ServeHTTP(writer, httptest.NewRequest(http.MethodDelete, "/"+h.token+apiPath+"/cache?"+query, nil))
		require.Equal(t, http.StatusBadRequest, writer.Code, query)
	}
	lease, err := h.transferLock(true)
	require.NoError(t, err)
	writer := httptest.NewRecorder()
	h.router.ServeHTTP(writer, httptest.NewRequest(http.MethodDelete, "/"+h.token+apiPath+"/cache?key=warm-0", nil))
	require.Equal(t, http.StatusServiceUnavailable, writer.Code)
	require.NoError(t, lease.Close())
	proof := AuditStore(context.Background(), h.dir, 0)
	require.False(t, proof.Partial, proof.Errors)
	require.Len(t, proof.Entries, 1)
	writer = httptest.NewRecorder()
	h.router.ServeHTTP(writer, httptest.NewRequest(http.MethodDelete, "/wrong-token"+apiPath+"/cache?key=warm-0", nil))
	require.Equal(t, http.StatusNotFound, writer.Code)
}

func TestExactDeleteRemovesCompletedVersionsButPreservesReservationAndPrefix(t *testing.T) {
	h := seedAuditStore(t, t.TempDir(), 2)
	db, err := h.openDB()
	require.NoError(t, err)
	completed := &Cache{Key: "warm-0", Version: "another-version", Complete: true, Size: 1}
	require.NoError(t, insertCache(db, completed))
	archive := h.storage.filename(completed.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0o755)) // #nosec G703 -- synthetic fixture archive.
	require.NoError(t, os.WriteFile(archive, []byte("x"), 0o600)) // #nosec G703 -- synthetic fixture archive.
	reservation := &Cache{Key: "warm-0", Version: "uploading"}
	require.NoError(t, insertCache(db, reservation))
	prefix := &Cache{Key: "warm-0-extra", Version: "uploading"}
	require.NoError(t, insertCache(db, prefix))
	require.NoError(t, db.Close())
	writer := httptest.NewRecorder()
	h.router.ServeHTTP(writer, httptest.NewRequest(http.MethodDelete, "/"+h.token+apiPath+"/cache?key=WARM-0", nil))
	require.Equal(t, http.StatusOK, writer.Code)
	var result exactDeleteReceipt
	require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &result))
	require.Equal(t, "WARM-0", result.Key)
	require.EqualValues(t, 2, result.DeletedCount)
	require.EqualValues(t, 81, result.Bytes)
	db, err = h.openDB()
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Get(reservation.ID, &Cache{}))
	require.NoError(t, db.Get(prefix.ID, &Cache{}))
}

func TestExactDeletionIntentRecoversAfterArchiveRemoval(t *testing.T) {
	dir := t.TempDir()
	h := seedAuditStore(t, dir, 2)
	db, err := h.openDB()
	require.NoError(t, err)
	var cache Cache
	require.NoError(t, db.FindOne(&cache, bolthold.Where("Key").Eq("warm-0")))
	require.NoError(t, db.Upsert(cache.ID, &DeletionIntent{Cache: cache, Reason: EvictionExplicit}))
	require.NoError(t, h.storage.Remove(cache.ID))
	require.NoError(t, db.Close())
	proof := MaintainStore(context.Background(), dir, DefaultPolicy())
	require.False(t, proof.Partial, proof.Errors)
	require.EqualValues(t, 1, proof.Retention.DeletedCount)
	require.Equal(t, EvictionExplicit, proof.Retention.Receipts[0].Reason)
	require.Len(t, proof.Entries, 1)
	require.Equal(t, "warm-1", proof.Entries[0].Key)
}
