package artifacts

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
)

var errArtifactLimit = errors.New("artifact service resource limit exceeded")
var errArtifactIncomplete = errors.New("artifact block is not completed")

// Local disk policy, not a claim about GitHub's hosted artifact quota. The block
// ceiling and count retain Azure's documented 4000 MiB / 50000-block semantics.
func defaultArtifactLimits() (ArtifactLimits, error) {
	limits := ArtifactLimits{MaxBlockBytes: 4000 << 20, MaxBlocks: 50000, MaxTotalBytes: 10 << 30}
	if value := os.Getenv("ACT_ARTIFACT_MAX_TOTAL_BYTES"); value != "" {
		total, err := strconv.ParseInt(value, 10, 64)
		if err != nil || total <= 0 {
			return limits, errors.New("ACT_ARTIFACT_MAX_TOTAL_BYTES must be a positive integer")
		}
		limits.MaxTotalBytes = total
	}
	return limits, nil
}

type artifactBlockState struct {
	lifecycle sync.RWMutex
	once      sync.Once
	store     atomic.Pointer[artifactBlockStore]
	err       error
}

type artifactStoredFile struct {
	mu       sync.RWMutex
	size     int64
	complete bool
	staged   bool
}

type artifactBlockStore struct {
	mu     sync.Mutex
	limits ArtifactLimits
	total  int64
	count  int
	files  map[string]*artifactStoredFile
}

func (r *artifactV4Routes) openBlockStore() (*os.Root, *artifactBlockStore, error) {
	if r.blockState == nil {
		return nil, nil, errors.New("artifact block state is not initialized")
	}
	root, err := os.OpenRoot(r.baseDir)
	if err != nil {
		return nil, nil, err
	}
	r.blockState.once.Do(func() {
		limits := r.limits
		if limits.MaxBlockBytes == 0 {
			limits, r.blockState.err = defaultArtifactLimits()
		}
		if r.blockState.err != nil {
			return
		}
		if limits.MaxBlockBytes <= 0 || limits.MaxBlocks <= 0 || limits.MaxTotalBytes <= 0 {
			r.blockState.err = errArtifactLimit
			return
		}
		store := &artifactBlockStore{limits: limits, files: make(map[string]*artifactStoredFile)}
		r.blockState.err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			store.files[name] = &artifactStoredFile{size: info.Size(), complete: true, staged: strings.Contains(filepath.ToSlash(name), "/.blocks/")}
			if info.Size() > limits.MaxTotalBytes-store.total {
				return errArtifactLimit
			}
			store.total += info.Size()
			if strings.Contains(filepath.ToSlash(name), "/.blocks/") {
				store.count++
			}
			if store.count > limits.MaxBlocks {
				return errArtifactLimit
			}
			return nil
		})
		r.blockState.store.Store(store)
	})
	if r.blockState.err != nil {
		_ = root.Close()
		return nil, nil, r.blockState.err
	}
	return root, r.blockState.store.Load(), nil
}

func (store *artifactBlockStore) prepareBlockDirectory(root *os.Root, relative string) error {
	// Root.MkdirAll can report EEXIST when concurrent uploads create the same
	// ancestors. Serialize directory setup only, leaving streamed uploads parallel.
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := root.MkdirAll(relative, 0755); err != nil {
		return err
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("artifact staging parent must be a directory without symlinks")
	}
	return nil
}

func (store *artifactBlockStore) file(relative string, block bool) (*artifactStoredFile, error) {
	relative = filepath.ToSlash(relative)
	store.mu.Lock()
	defer store.mu.Unlock()
	if entry, ok := store.files[relative]; ok {
		return entry, nil
	}
	if block && store.count >= store.limits.MaxBlocks {
		return nil, errArtifactLimit
	}
	entry := &artifactStoredFile{staged: block}
	store.files[relative] = entry
	if block {
		store.count++
	}
	return entry, nil
}

type boundedArtifactWriter struct {
	file  *os.File
	store *artifactBlockStore
	entry *artifactStoredFile
	limit int64
}

func (store *artifactBlockStore) openWriter(root *os.Root, relative string, entry *artifactStoredFile, limit int64) (*boundedArtifactWriter, error) {
	if info, err := root.Lstat(relative); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("artifact file must not be a symlink")
	}
	file, err := root.OpenFile(relative, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.total -= entry.size
	entry.size = 0
	entry.complete = false
	store.mu.Unlock()
	return &boundedArtifactWriter{file: file, store: store, entry: entry, limit: limit}, nil
}

func (writer *boundedArtifactWriter) Write(data []byte) (int, error) {
	store := writer.store
	store.mu.Lock()
	available := min(writer.limit-writer.entry.size, store.limits.MaxTotalBytes-store.total)
	size := int64(len(data))
	if size > available {
		store.mu.Unlock()
		return 0, errArtifactLimit
	}
	// Reserve before I/O so concurrent uploads cannot exceed the shared budget.
	store.total += size
	writer.entry.size += size
	store.mu.Unlock()
	written, err := writer.file.Write(data)
	if int64(written) != size {
		store.mu.Lock()
		store.total -= size - int64(written)
		writer.entry.size -= size - int64(written)
		store.mu.Unlock()
	}
	return written, err
}

func (writer *boundedArtifactWriter) finish(copyErr error) error {
	if copyErr != nil {
		if err := writer.file.Truncate(0); err != nil {
			_ = writer.file.Close()
			return err
		}
		writer.store.mu.Lock()
		writer.store.total -= writer.entry.size
		writer.entry.size = 0
		writer.store.mu.Unlock()
	}
	closeErr := writer.file.Close()
	writer.entry.complete = copyErr == nil && closeErr == nil
	return closeErr
}

func artifactBlockError(ctx *ArtifactContext, err error) {
	if errors.Is(err, errArtifactIncomplete) {
		ctx.Error(http.StatusBadRequest)
		return
	}
	if errors.Is(err, errArtifactLimit) {
		ctx.Error(http.StatusRequestEntityTooLarge)
		return
	}
	// Report the filesystem operation and errno, never its path or request URL.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		log.WithField("operation", pathErr.Op).WithField("reason", pathErr.Err).Error("artifact block filesystem operation failed")
	}
	// Do not include paths, signed URLs, or request bodies in client diagnostics.
	ctx.Error(http.StatusInternalServerError)
}

// Lock order: lifecycle lease, staged files in path order, archive, quota mutex.
// Never hold the quota mutex while waiting for a file lock.
type artifactBlockSnapshot struct{ files []*artifactStoredFile }

func (snapshot *artifactBlockSnapshot) unlock() {
	for i := len(snapshot.files) - 1; i >= 0; i-- {
		snapshot.files[i].mu.RUnlock()
	}
}

func (store *artifactBlockStore) snapshotCompletedBlocks(paths []string) (*artifactBlockSnapshot, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	snapshot := &artifactBlockSnapshot{}
	store.mu.Lock()
	previous := ""
	for _, path := range sorted {
		key := filepath.ToSlash(path)
		if key == previous {
			continue
		}
		previous = key
		entry, ok := store.files[key]
		if !ok {
			store.mu.Unlock()
			return nil, errArtifactIncomplete
		}
		snapshot.files = append(snapshot.files, entry)
	}
	store.mu.Unlock()
	for i, entry := range snapshot.files {
		entry.mu.RLock()
		if !entry.complete {
			for j := i; j >= 0; j-- {
				snapshot.files[j].mu.RUnlock()
			}
			return nil, errArtifactIncomplete
		}
	}
	return snapshot, nil
}

func (store *artifactBlockStore) lookup(relative string) *artifactStoredFile {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.files[filepath.ToSlash(relative)]
}

// Call only with the exclusive lifecycle lease: no upload, commit or download
// can retain a file or its accounting while the corresponding tree is removed.
func (store *artifactBlockStore) forget(relative string, tree bool) {
	key := filepath.ToSlash(relative)
	store.mu.Lock()
	defer store.mu.Unlock()
	for name, entry := range store.files {
		if name != key && (!tree || !strings.HasPrefix(name, key+"/")) {
			continue
		}
		store.total -= entry.size
		if entry.staged {
			store.count--
		}
		delete(store.files, name)
	}
}
