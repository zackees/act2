package runner

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nektos/act/pkg/container"
)

// Restoration is local resource policy: at most 1 GiB and 100,000 entries per
// owned root. All archives are staged and validated before either root changes.
const hostRestoreBytes int64 = 1 << 30
const hostRestoreEntries = 100000

type hostRestoreEntry struct {
	path string
	info fs.FileInfo
}
type hostRestoreDirectory struct {
	path string
	mode fs.FileMode
}
type restoreDirectoryInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (info restoreDirectoryInfo) Mode() fs.FileMode { return os.ModeDir | info.mode }

type hostRestoreMutation struct {
	path, backup string
	installed    bool
	modeChanged  bool
	oldMode      fs.FileMode
}
type hostRestorePlan struct {
	root          *os.Root
	scratch       string
	originalTime  time.Time
	entries       []hostRestoreEntry
	mutations     []hostRestoreMutation
	directories   map[string]hostRestoreDirectory
	stagedEntries int
}

func restoreHostDockerFiles(ctx context.Context, action container.Container, paths []hostDockerPath) (result error) {
	plans := []*hostRestorePlan{}
	defer func() {
		for _, plan := range plans {
			result = errors.Join(result, plan.cleanup())
		}
	}()
	for _, path := range paths {
		plan, err := prepareHostRestore(ctx, action, path)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	return applyHostRestores(ctx, plans)
}
func prepareHostRestore(ctx context.Context, action container.Container, path hostDockerPath) (*hostRestorePlan, error) {
	info, err := os.Lstat(path.host)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("host restore root must be a directory without symlinks")
	}
	root, err := os.OpenRoot(path.host)
	if err != nil {
		return nil, err
	}
	plan := &hostRestorePlan{root: root, originalTime: info.ModTime(), scratch: ".act-restore-" + rand.Text()}
	if err = root.Mkdir(plan.scratch, 0700); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err = root.Mkdir(plan.scratch+"/incoming", 0700); err != nil {
		_ = plan.cleanup()
		return nil, err
	}
	archive, err := action.GetContainerArchive(ctx, path.action+"/.")
	if err == nil {
		err = plan.stage(ctx, archive)
		err = errors.Join(err, archive.Close())
	}
	if err != nil {
		err = errors.Join(err, plan.cleanup())
		return nil, err
	}
	return plan, nil
}
func restoreRelative(name string) (string, error) {
	name = filepath.FromSlash(name)
	clean := filepath.Clean(name)
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("Docker archive path escapes owned root")
	}
	return clean, nil
}
func (plan *hostRestorePlan) stage(ctx context.Context, archive io.Reader) error {
	plan.directories = map[string]hostRestoreDirectory{}
	reader := tar.NewReader(io.LimitReader(archive, hostRestoreBytes+hostRestoreEntries*1024))
	total := int64(0)
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if count >= hostRestoreEntries {
			return errors.New("Docker archive entry limit exceeded")
		}
		relative, err := restoreRelative(header.Name)
		if err != nil {
			return err
		}
		if relative == "." {
			if header.Typeflag != tar.TypeDir {
				return errors.New("invalid Docker archive root")
			}
			continue
		}
		if strings.Split(relative, string(filepath.Separator))[0] == plan.scratch {
			return errors.New("Docker archive uses reserved restore path")
		}
		target := filepath.Join(plan.scratch, "incoming", relative)
		if err = plan.stageEntry(reader, header, relative, target, &total); err != nil {
			return err
		}
	}
	if err := plan.validateStageLinks(); err != nil {
		return err
	}
	return fs.WalkDir(plan.root.FS(), plan.scratch+"/incoming", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == plan.scratch+"/incoming" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(plan.scratch+"/incoming", name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if directory, ok := plan.directories[relative]; ok {
				info = restoreDirectoryInfo{FileInfo: info, mode: directory.mode}
			}
		}
		plan.entries = append(plan.entries, hostRestoreEntry{path: relative, info: info})
		return nil
	})
}

// Resolve links relative to the staged tree, including composed internal links.
// A dangling internal link is valid; resolution outside the tree is rejected.
func (plan *hostRestorePlan) validateStageLinks() error {
	root, err := plan.root.OpenRoot(plan.scratch + "/incoming")
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		_, err = root.Stat(name)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}
func (plan *hostRestorePlan) stageEntry(reader io.Reader, header *tar.Header, relative, target string, total *int64) error {
	parent := filepath.Dir(target)
	if err := plan.checkStagingParents(parent); err != nil {
		return err
	}
	if err := plan.stageDirectories(parent); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if err := plan.stageDirectories(target); err != nil {
			return err
		}
		plan.directories[relative] = hostRestoreDirectory{path: relative, mode: fs.FileMode(header.Mode) & 0777}
		return nil
	// Older tar writers use a NUL flag for regular files.
	case tar.TypeReg, 0:
		if header.Size < 0 || header.Size > hostRestoreBytes-*total {
			return errors.New("Docker archive byte limit exceeded")
		}
		if err := plan.reserveStageEntry(); err != nil {
			return err
		}
		file, err := plan.root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(header.Mode)&0777)
		if err != nil {
			return err
		}
		written, copyErr := io.CopyN(file, reader, header.Size)
		*total += written
		if err = errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		if err = plan.root.Chmod(target, fs.FileMode(header.Mode)&0777); err != nil {
			return err
		}
		if !header.ModTime.IsZero() {
			return plan.root.Chtimes(target, header.ModTime, header.ModTime)
		}
		return nil
	case tar.TypeSymlink:
		link, err := restoreRelative(filepath.Dir(relative) + string(filepath.Separator) + header.Linkname)
		if err != nil || filepath.IsAbs(header.Linkname) || link == "." {
			return errors.New("Docker archive symlink escapes owned root")
		}
		if err := plan.reserveStageEntry(); err != nil {
			return err
		}
		return plan.root.Symlink(header.Linkname, target)
	default:
		return errors.New("unsupported Docker archive entry type")
	}
}
func (plan *hostRestorePlan) reserveStageEntry() error {
	if plan.stagedEntries >= hostRestoreEntries {
		return errors.New("Docker archive entry limit exceeded")
	}
	plan.stagedEntries++
	return nil
}
func (plan *hostRestorePlan) stageDirectories(parent string) error {
	components := strings.Split(parent, string(filepath.Separator))
	if len(components) > 128 {
		return errors.New("Docker archive path depth exceeded")
	}
	name := ""
	for _, component := range components {
		name = filepath.Join(name, component)
		info, err := plan.root.Lstat(name)
		if err == nil {
			if !info.IsDir() {
				return errors.New("Docker archive parent must be a directory without symlinks")
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err = plan.reserveStageEntry(); err != nil {
			return err
		}
		if err = plan.root.Mkdir(name, 0700); err != nil {
			return err
		}
	}
	return nil
}
func (plan *hostRestorePlan) checkStagingParents(parent string) error {
	for name := parent; name != "."; name = filepath.Dir(name) {
		info, err := plan.root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("Docker archive parent must be a directory without symlinks")
		}
	}
	return nil
}
func applyHostRestores(ctx context.Context, plans []*hostRestorePlan) error {
	for _, plan := range plans {
		if err := plan.apply(ctx); err != nil {
			for index := len(plans) - 1; index >= 0; index-- {
				err = errors.Join(err, plans[index].rollback())
			}
			return err
		}
	}
	return nil
}
func (plan *hostRestorePlan) apply(ctx context.Context) error {
	desired := map[string]hostRestoreEntry{}
	for _, entry := range plan.entries {
		desired[entry.path] = entry
	}
	missing := []string{}
	count := 0
	if err := fs.WalkDir(plan.root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == plan.scratch {
			return fs.SkipDir
		}
		if name == "." {
			return nil
		}
		count++
		if count > hostRestoreEntries {
			return errors.New("host restore entry limit exceeded")
		}
		if _, ok := desired[name]; !ok {
			missing = append(missing, name)
			if entry.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, name := range missing {
		if err := plan.backup(name); err != nil {
			return err
		}
	}
	sort.Slice(plan.entries, func(left, right int) bool {
		a, b := plan.entries[left], plan.entries[right]
		if a.info.IsDir() != b.info.IsDir() {
			return a.info.IsDir()
		}
		return a.path < b.path
	})
	for _, entry := range plan.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := plan.install(entry); err != nil {
			return err
		}
	}
	// Apply directory modes after children, so restrictive modes cannot prevent
	// installation. Record existing modes for rollback across both roots.
	for index := len(plan.entries) - 1; index >= 0; index-- {
		entry := plan.entries[index]
		if !entry.info.IsDir() {
			continue
		}
		info, err := plan.root.Lstat(entry.path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() == entry.info.Mode().Perm() {
			continue
		}
		plan.mutations = append(plan.mutations, hostRestoreMutation{path: entry.path, modeChanged: true, oldMode: info.Mode().Perm()})
		if err = plan.root.Chmod(entry.path, entry.info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
func (plan *hostRestorePlan) backup(name string) error {
	backup := fmt.Sprintf("%s/backup-%d", plan.scratch, len(plan.mutations))
	if err := plan.root.Rename(name, backup); err != nil {
		return err
	}
	plan.mutations = append(plan.mutations, hostRestoreMutation{path: name, backup: backup})
	return nil
}
func (plan *hostRestorePlan) install(entry hostRestoreEntry) error {
	old, err := plan.root.Lstat(entry.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	staged := filepath.Join(plan.scratch, "incoming", entry.path)
	if err == nil {
		same, compareErr := plan.unchanged(entry, old, staged)
		if compareErr != nil {
			return compareErr
		}
		if same {
			return nil
		}
		if err = plan.backup(entry.path); err != nil {
			return err
		}
	}
	if entry.info.IsDir() {
		err = plan.root.Mkdir(entry.path, 0755)
	} else {
		err = plan.root.Rename(staged, entry.path)
	}
	if err != nil {
		return err
	}
	plan.mutations = append(plan.mutations, hostRestoreMutation{path: entry.path, installed: true})
	return nil
}
func (plan *hostRestorePlan) unchanged(entry hostRestoreEntry, old fs.FileInfo, staged string) (bool, error) {
	if entry.info.IsDir() && old.IsDir() {
		return true, nil
	}
	if entry.info.Mode()&os.ModeSymlink != 0 && old.Mode()&os.ModeSymlink != 0 {
		a, err := plan.root.Readlink(entry.path)
		if err != nil {
			return false, err
		}
		b, err := plan.root.Readlink(staged)
		return a == b, err
	}
	if !entry.info.Mode().IsRegular() || !old.Mode().IsRegular() || entry.info.Size() != old.Size() || entry.info.Mode().Perm() != old.Mode().Perm() {
		return false, nil
	}
	a, err := restoreFileHash(plan.root, entry.path)
	if err != nil {
		return false, err
	}
	b, err := restoreFileHash(plan.root, staged)
	return a == b, err
}
func restoreFileHash(root *os.Root, name string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	file, err := root.Open(name)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}
func (plan *hostRestorePlan) rollback() error {
	var result error
	for index := len(plan.mutations) - 1; index >= 0; index-- {
		mutation := plan.mutations[index]
		if mutation.modeChanged {
			result = errors.Join(result, plan.root.Chmod(mutation.path, mutation.oldMode))
		}
		if mutation.installed {
			result = errors.Join(result, plan.root.RemoveAll(mutation.path))
		}
		if mutation.backup != "" {
			result = errors.Join(result, plan.root.Rename(mutation.backup, mutation.path))
		}
	}
	return result
}
func (plan *hostRestorePlan) cleanup() error {
	err := plan.root.RemoveAll(plan.scratch)
	err = errors.Join(err, plan.root.Chtimes(".", plan.originalTime, plan.originalTime))
	return errors.Join(err, plan.root.Close())
}
