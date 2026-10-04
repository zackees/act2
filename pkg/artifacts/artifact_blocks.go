package artifacts

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

var errArtifactLimit = errors.New("artifact service resource limit exceeded")

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
	once  sync.Once
	store *artifactBlockStore
	err   error
}

type artifactStoredFile struct {
	mu   sync.Mutex
	size int64
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
			store.files[name] = &artifactStoredFile{size: info.Size()}
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
		r.blockState.store = store
	})
	if r.blockState.err != nil {
		_ = root.Close()
		return nil, nil, r.blockState.err
	}
	return root, r.blockState.store, nil
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
	store.mu.Lock()
	defer store.mu.Unlock()
	if entry, ok := store.files[relative]; ok {
		return entry, nil
	}
	if block && store.count >= store.limits.MaxBlocks {
		return nil, errArtifactLimit
	}
	entry := &artifactStoredFile{}
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
	return writer.file.Close()
}

func artifactBlockError(ctx *ArtifactContext, err error) {
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
