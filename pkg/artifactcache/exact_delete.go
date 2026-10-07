package artifactcache

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/julienschmidt/httprouter"
	"github.com/timshannon/bolthold"
)

type exactDeleteReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	Key           string `json:"key"`
	DeletedCount  uint64 `json:"deleted_count"`
	Bytes         int64  `json:"reclaimed_archive_bytes"`
}

// DELETE /_apis/artifactcache/cache?key=<exact-key>. The server token in the
// route grants access only to this namespace. Completed versions are removed;
// in-flight reservations and other keys are preserved. No prefix matching.
func (h *Handler) exactDelete(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	query, err := parseExactDeleteKey(r)
	if err != nil {
		h.responseJSON(w, r, http.StatusBadRequest, err)
		return
	}
	transfer, err := h.transferLock(false)
	if err != nil {
		h.responseJSON(w, r, http.StatusServiceUnavailable, fmt.Errorf("cache transfer coordination busy"))
		return
	}
	defer transfer.Close()
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, http.StatusInternalServerError, err)
		return
	}
	defer db.Close()
	var caches []*Cache
	if err := db.Find(&caches, bolthold.Where("Key").Eq(strings.ToLower(query)).And("Complete").Eq(true).Limit(auditMaxFiles+1)); err != nil {
		h.responseJSON(w, r, http.StatusInternalServerError, err)
		return
	}
	if len(caches) > auditMaxFiles {
		h.responseJSON(w, r, http.StatusServiceUnavailable, fmt.Errorf("exact-key deletion limit exceeded"))
		return
	}
	var report RetentionReport
	for _, cache := range caches {
		if err := r.Context().Err(); err != nil {
			return
		}
		if err := h.deleteCache(db, cache, EvictionExplicit, &report); err != nil {
			h.responseJSON(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	h.responseJSON(w, r, http.StatusOK, exactDeleteReceipt{1, query, report.DeletedCount, report.ReclaimedArchiveBytes})
}

func parseExactDeleteKey(r *http.Request) (string, error) {
	if len(r.URL.RawQuery) > 2048 {
		return "", fmt.Errorf("exact-key deletion query exceeds its size bound")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	keys := values["key"]
	if err != nil || len(values) != 1 || len(keys) != 1 {
		return "", fmt.Errorf("exact-key deletion requires one key and no other parameters")
	}
	key := keys[0]
	if len(key) == 0 || len(key) > 512 || !utf8.ValidString(key) || strings.TrimSpace(key) != key || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid exact cache key")
	}
	return key, nil
}
