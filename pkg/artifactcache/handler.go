package artifactcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"

	"github.com/nektos/act/pkg/common"
)

const (
	apiPath = "/_apis/artifactcache"
)

type Handler struct {
	dir      string
	storage  *Storage
	router   *httprouter.Router
	listener net.Listener
	server   *http.Server
	logger   logrus.FieldLogger

	gcing            atomic.Bool
	gcAt             time.Time
	policy           Policy
	stopMaintenance  chan struct{}
	maintenanceDone  chan struct{}
	cohortLease      transferLease
	retentionOnClose *CohortReport

	outboundIP        string
	customExternalURL string
	token             string
}

func StartHandler(dir, customExternalURL string, outboundIP string, port uint16, logger logrus.FieldLogger) (*Handler, error) {
	return StartHandlerWithPolicy(dir, customExternalURL, outboundIP, port, logger, DefaultPolicy())
}

// StartHandlerWithPolicy starts a cache server with periodic retention. All
// servers sharing a namespace must participate in transfer coordination.
func StartHandlerWithPolicy(dir, customExternalURL string, outboundIP string, port uint16, logger logrus.FieldLogger, policy Policy) (*Handler, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	h := &Handler{policy: policy}

	if logger == nil {
		discard := logrus.New()
		discard.Out = io.Discard
		logger = discard
	}
	logger = logger.WithField("module", "artifactcache")
	h.logger = logger

	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".cache", "actcache")
	}
	lease, err := enrollCohort(dir, policy.CohortRoot)
	if err != nil {
		return nil, err
	}
	h.cohortLease = lease
	started := false
	defer func() {
		if !started && lease != nil {
			_ = lease.Close()
		}
	}()
	//nolint:gosec // This directory is explicitly selected by the local CLI caller, not an HTTP request.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	h.dir = dir
	if err := h.prepareTransferLock(); err != nil {
		return nil, err
	}

	storage, err := NewStorage(filepath.Join(dir, "cache"))
	if err != nil {
		return nil, err
	}
	h.storage = storage

	if customExternalURL != "" {
		h.customExternalURL = customExternalURL
	}

	if outboundIP != "" {
		h.outboundIP = outboundIP
	} else if ip := common.GetOutboundIP(); ip == nil {
		return nil, fmt.Errorf("unable to determine outbound IP address")
	} else {
		h.outboundIP = ip.String()
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate auth token: %w", err)
	}
	h.token = hex.EncodeToString(tokenBytes)

	router := httprouter.New()
	base := "/" + h.token + apiPath
	router.GET(base+"/cache", h.middleware(h.find))
	router.POST(base+"/caches", h.middleware(h.reserve))
	router.PATCH(base+"/caches/:id", h.middleware(h.upload))
	router.POST(base+"/caches/:id", h.middleware(h.commit))
	router.GET(base+"/artifacts/:id", h.middleware(h.get))
	router.POST(base+"/clean", h.middleware(h.clean))

	h.router = router

	h.gcCache()

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", h.outboundIP, port))
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           router,
	}
	go func() {
		if err := server.Serve(listener); err != nil && errors.Is(err, net.ErrClosed) {
			logger.Errorf("http serve: %v", err)
		}
	}()
	h.listener = listener
	h.server = server
	h.startMaintenance()
	started = true

	return h, nil
}

func (h *Handler) GetActualPort() int {
	return h.listener.Addr().(*net.TCPAddr).Port
}

func (h *Handler) ExternalURL() string {
	if h.customExternalURL != "" {
		return h.customExternalURL + "/" + h.token
	}
	return fmt.Sprintf("http://%s:%d/%s", h.outboundIP, h.GetActualPort(), h.token)
}

func (h *Handler) Close() error {
	return h.CloseContext(context.Background())
}

// CloseContext closes the server and bounds optional aggregate maintenance
// with the caller context and the maintenance operation deadline.
func (h *Handler) CloseContext(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if h.stopMaintenance != nil {
		close(h.stopMaintenance)
		<-h.maintenanceDone
		h.stopMaintenance = nil
	}
	var retErr error
	if h.server != nil {
		err := h.server.Close()
		if err != nil {
			retErr = err
		}
		h.server = nil
	}
	if h.listener != nil {
		err := h.listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		if err != nil {
			retErr = err
		}
		h.listener = nil
	}
	if h.cohortLease != nil {
		retErr = errors.Join(retErr, h.cohortLease.Close())
		h.cohortLease = nil
		h.maintainIdleCohort(ctx)
	}
	return retErr
}

func (h *Handler) openDB() (*bolthold.Store, error) {
	return bolthold.Open(filepath.Join(h.dir, "bolt.db"), 0o644, &bolthold.Options{
		Encoder: json.Marshal,
		Decoder: json.Unmarshal,
		Options: &bbolt.Options{
			Timeout:      5 * time.Second,
			NoGrowSync:   bbolt.DefaultOptions.NoGrowSync,
			FreelistType: bbolt.DefaultOptions.FreelistType,
		},
	})
}

// GET /_apis/artifactcache/cache
func (h *Handler) find(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	// cache keys are case insensitive
	for i, key := range keys {
		keys[i] = strings.ToLower(key)
	}
	version := r.URL.Query().Get("version")

	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()

	cache, err := findCache(db, keys, version)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	if cache == nil {
		h.responseJSON(w, r, 204)
		return
	}

	if ok, err := h.storage.Exist(cache.ID); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	} else if !ok {
		// Preserve metadata and any deletion intent for coordinated maintenance.
		// A lookup miss must not hide damage or orphan interrupted cleanup.
		h.responseJSON(w, r, 204)
		return
	}
	cache.UsedAt = time.Now().Unix()
	if err := db.Update(cache.ID, cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	h.responseJSON(w, r, 200, map[string]any{
		"result":          "hit",
		"archiveLocation": fmt.Sprintf("%s%s/artifacts/%d", h.ExternalURL(), apiPath, cache.ID),
		"cacheKey":        cache.Key,
	})
}

// POST /_apis/artifactcache/caches
func (h *Handler) reserve(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	api := &Request{}
	if err := json.NewDecoder(r.Body).Decode(api); err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	// cache keys are case insensitive
	api.Key = strings.ToLower(api.Key)

	cache := api.ToCache()
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()

	now := time.Now().Unix()
	cache.CreatedAt = now
	cache.UsedAt = now
	if err := insertCache(db, cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	h.responseJSON(w, r, 200, map[string]any{
		"cacheId": cache.ID,
	})
}

// PATCH /_apis/artifactcache/caches/:id
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseUint(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache := &Cache{}
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()
	if err := db.Get(id, cache); err != nil {
		if errors.Is(err, bolthold.ErrNotFound) {
			h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
			return
		}
		h.responseJSON(w, r, 500, err)
		return
	}

	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}
	db.Close()
	start, _, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	if err := h.storage.Write(cache.ID, start, r.Body); err != nil {
		h.responseJSON(w, r, 500, err)
	}
	h.useCache(id)
	h.responseJSON(w, r, 200)
}

// POST /_apis/artifactcache/caches/:id
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache := &Cache{}
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()
	if err := db.Get(id, cache); err != nil {
		if errors.Is(err, bolthold.ErrNotFound) {
			h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
			return
		}
		h.responseJSON(w, r, 500, err)
		return
	}

	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}

	db.Close()

	size, err := h.storage.Commit(cache.ID, cache.Size)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	// write real size back to cache, it may be different from the current value when the request doesn't specify it.
	cache.Size = size

	db, err = h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()

	cache.Complete = true
	if err := db.Update(cache.ID, cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}

	h.responseJSON(w, r, 200)
}

// GET /_apis/artifactcache/artifacts/:id
func (h *Handler) get(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseUint(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	h.useCache(id)
	h.storage.Serve(w, r, id)
}

// POST /_apis/artifactcache/clean
func (h *Handler) clean(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	// TODO: don't support force deleting cache entries
	// see: https://docs.github.com/en/actions/using-workflows/caching-dependencies-to-speed-up-workflows#force-deleting-cache-entries

	h.responseJSON(w, r, 200)
}

func (h *Handler) middleware(handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		transfer, err := h.transferLock(true)
		if err != nil {
			h.responseJSON(w, r, http.StatusServiceUnavailable, fmt.Errorf("cache transfer coordination: %w", err))
			return
		}
		// Always acquire transfer coordination before opening metadata.
		// A defer also releases the OS lock if the handler panics.
		func() {
			defer func() {
				if err := transfer.Close(); err != nil {
					h.logger.Warnf("release transfer coordination: %v", err)
				}
			}()
			h.logger.Debugf("%s %s", r.Method, r.RequestURI)
			handler(w, r, params)
		}()
		go h.gcCache()
	}
}

// if not found, return (nil, nil) instead of an error.
func findCache(db *bolthold.Store, keys []string, version string) (*Cache, error) {
	cache := &Cache{}
	for _, prefix := range keys {
		// if a key in the list matches exactly, don't return partial matches
		if err := db.FindOne(cache,
			bolthold.Where("Key").Eq(prefix).
				And("Version").Eq(version).
				And("Complete").Eq(true).
				SortBy("CreatedAt").Reverse()); err == nil || !errors.Is(err, bolthold.ErrNotFound) {
			if err != nil {
				return nil, fmt.Errorf("find cache: %w", err)
			}
			return cache, nil
		}
		prefixPattern := fmt.Sprintf("^%s", regexp.QuoteMeta(prefix))
		re, err := regexp.Compile(prefixPattern)
		if err != nil {
			continue
		}
		if err := db.FindOne(cache,
			bolthold.Where("Key").RegExp(re).
				And("Version").Eq(version).
				And("Complete").Eq(true).
				SortBy("CreatedAt").Reverse()); err != nil {
			if errors.Is(err, bolthold.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("find cache: %w", err)
		}
		return cache, nil
	}
	return nil, nil
}

func insertCache(db *bolthold.Store, cache *Cache) error {
	if err := db.Insert(bolthold.NextSequence(), cache); err != nil {
		return fmt.Errorf("insert cache: %w", err)
	}
	// write back id to db
	if err := db.Update(cache.ID, cache); err != nil {
		return fmt.Errorf("write back id to db: %w", err)
	}
	return nil
}

func (h *Handler) useCache(id uint64) {
	db, err := h.openDB()
	if err != nil {
		return
	}
	defer db.Close()
	cache := &Cache{}
	if err := db.Get(id, cache); err != nil {
		return
	}
	cache.UsedAt = time.Now().Unix()
	_ = db.Update(cache.ID, cache)
}

func (h *Handler) responseJSON(w http.ResponseWriter, r *http.Request, code int, v ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var data []byte
	if len(v) == 0 || v[0] == nil {
		data, _ = json.Marshal(struct{}{})
	} else if err, ok := v[0].(error); ok {
		h.logger.Errorf("%v %v: %v", r.Method, r.RequestURI, err)
		data, _ = json.Marshal(map[string]any{
			"error": err.Error(),
		})
	} else {
		data, _ = json.Marshal(v[0])
	}
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func parseContentRange(s string) (int64, int64, error) {
	// support the format like "bytes 11-22/*" only
	s, _, _ = strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	s1, s2, _ := strings.Cut(s, "-")

	start, err := strconv.ParseInt(s1, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	stop, err := strconv.ParseInt(s2, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return start, stop, nil
}
