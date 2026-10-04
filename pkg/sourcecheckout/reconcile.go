package sourcecheckout

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type preparedEntry struct {
	entry Entry
	data  []byte
}

func validateEnvelope(e Envelope, l Limits) error {
	if e.Version != 1 || len(e.Entries) > l.Files || len(e.ContentID) != 64 {
		return fmt.Errorf("invalid source envelope")
	}
	if err := validBinding(e.Binding, true); err != nil {
		return err
	}
	total := int64(0)
	previous := ""
	seen := make(map[string]bool)
	for _, entry := range e.Entries {
		if err := allowed(entry.Path, l); err != nil {
			return err
		}
		if entry.Path <= previous || entry.Size < 0 || entry.Size > l.FileBytes || entry.Size > l.TotalBytes-total {
			return fmt.Errorf("unordered or oversized source inventory")
		}

		if seen[foldSourcePath(entry.Path)] {
			return fmt.Errorf("source path case collision")
		}
		seen[foldSourcePath(entry.Path)] = true
		previous = entry.Path
		total += entry.Size
		if err := validateEntry(entry, l); err != nil {
			return err
		}
	}
	if err := validateCompleteAncestors(e.Entries, seen); err != nil {
		return err
	}
	if envelopeID(e) != e.ContentID {
		return fmt.Errorf("source envelope identity mismatch")
	}
	return nil
}
func prepare(staging string, next Envelope, l Limits) ([]preparedEntry, error) {
	result := make([]preparedEntry, 0, len(next.Entries))
	for _, entry := range next.Entries {
		if err := realParents(staging, entry.Path); err != nil {
			return nil, err
		}
		filename := filepath.Join(staging, entry.Path)
		info, err := os.Lstat(filename)
		if err != nil {
			return nil, err
		}
		item := preparedEntry{entry: entry}
		if entry.Kind == "symlink" {
			if info.Mode()&os.ModeSymlink == 0 {
				return nil, fmt.Errorf("staging source type mismatch")
			}
			target, err := os.Readlink(filename)
			if err != nil {
				return nil, err
			}
			if target != entry.Link {
				return nil, fmt.Errorf("staging source link mismatch")
			}
		} else {
			if !info.Mode().IsRegular() || (info.Mode()&0111 != 0) != entry.Executable {
				return nil, fmt.Errorf("staging source mode mismatch")
			}
			item.data, err = readFile(filename, l.FileBytes)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(item.data)
			if int64(len(item.data)) != entry.Size || hex.EncodeToString(sum[:]) != entry.SHA256 {
				return nil, fmt.Errorf("staging source content mismatch")
			}
		}
		result = append(result, item)
	}
	return result, nil
}
func entriesByPath(e Envelope) map[string]Entry {
	result := make(map[string]Entry, len(e.Entries))
	for _, entry := range e.Entries {
		result[entry.Path] = entry
	}
	return result
}

// checkDestination permits absent ancestors and donor-owned leaf transitions,
// but never follows symlinks or replaces unlisted destination files.
func checkDestination(root, name string, previous map[string]Entry) error {
	current := root
	parts := strings.Split(name, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		relative := strings.Join(parts[:index+1], "/")
		if info.IsDir() {
			continue
		}
		old, owned := previous[relative]
		if !owned {
			return fmt.Errorf("unlisted destination collision: %s", relative)
		}
		if old.Kind == "file" && !info.Mode().IsRegular() || old.Kind == "symlink" && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("donor destination type mismatch: %s", relative)
		}
		if index < len(parts)-1 {
			return nil
		} // validated donor leaf is removed before parents are created
	}
	return nil
}
func checkDirectoryReplacement(root, name string, previous map[string]Entry, next map[string]Entry, directories map[string]bool) error {
	for parent := filepath.ToSlash(filepath.Dir(name)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
		if _, leaf := previous[parent]; leaf {
			return nil
		}
	}
	filename := filepath.Join(root, name)
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	count := 0
	return filepath.WalkDir(filename, func(p string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 10000 {
			return fmt.Errorf("source directory scan exceeds bound")
		}
		relative, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if item.IsDir() {
			if directories[relative] {
				return nil
			}
			return fmt.Errorf("unlisted source directory: %s", relative)
		}
		if _, owned := previous[relative]; !owned {
			return fmt.Errorf("unlisted directory content: %s", relative)
		}
		if _, kept := next[relative]; kept {
			return fmt.Errorf("conflicting requested directory content")
		}
		return nil
	})
}
func identical(root string, item preparedEntry) (bool, error) {
	filename := filepath.Join(root, item.entry.Path)
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if item.entry.Kind == "symlink" {
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		target, err := os.Readlink(filename)
		return target == item.entry.Link, err
	}
	if !info.Mode().IsRegular() || info.Size() != item.entry.Size || (info.Mode()&0111 != 0) != item.entry.Executable {
		return false, nil
	}
	data, err := readFile(filename, item.entry.Size)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == item.entry.SHA256, nil
}

// removeEmptyTree removes only empty directories after explicit donor leaves
// have been deleted. Walk rejects every remaining file, including symlinks.
func removeEmptyTree(filename string) error {
	var directories []string
	err := filepath.WalkDir(filename, func(p string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !item.IsDir() {
			return fmt.Errorf("unlisted source content remains")
		}
		directories = append(directories, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Remove(directories[i]); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile requires exclusive ownership of the writable destination and staging
// roots. It authenticates neither cache provenance nor Git history. All envelope,
// staging and destination safety checks finish before the first mutation.
func Reconcile(destination, staging string, previous, next Envelope, l Limits) (Result, error) {
	result := Result{}
	if err := validLimits(l); err != nil {
		return result, err
	}
	if err := validateRoots(destination, staging); err != nil {
		return result, err
	}
	for _, e := range []Envelope{previous, next} {
		if err := validateEnvelope(e, l); err != nil {
			return result, err
		}
	}
	prepared, err := prepare(staging, next, l)
	if err != nil {
		return result, err
	}
	old, wanted := entriesByPath(previous), entriesByPath(next)
	if err := preflightDestination(destination, previous, next, old, wanted); err != nil {
		return result, err
	}
	if err := validateLinks(staging, destination, previous, next, l); err != nil {
		return result, err
	}
	return applyPrepared(destination, previous, wanted, prepared)
}

func applyPrepared(destination string, previous Envelope, wanted map[string]Entry, prepared []preparedEntry) (Result, error) {
	result := Result{}
	var deletions []string
	for _, entry := range previous.Entries {
		replacement, kept := wanted[entry.Path]
		if !kept || replacement.Kind != entry.Kind {
			deletions = append(deletions, entry.Path)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(deletions)))
	for _, name := range deletions {
		filename := filepath.Join(destination, name)
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			return result, err
		}
		result.Deleted++
	}
	for _, item := range prepared {
		same, err := identical(destination, item)
		if err != nil {
			return result, err
		}
		if same {
			result.Unchanged++
			continue
		}
		if err := applyEntry(destination, item); err != nil {
			return result, err
		}
		result.Written++
	}
	return result, nil
}

// Replacement never writes through a donor hardlink into protected build state.
func replaceFile(filename string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(filename), ".source-checkout-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filename)
}

func validateEntry(entry Entry, l Limits) error {
	switch entry.Kind {
	case "file":
		if len(entry.SHA256) != 64 || entry.Link != "" {
			return fmt.Errorf("invalid source file")
		}
		_, err := hex.DecodeString(entry.SHA256)
		return err
	case "symlink":
		if !safeLink(entry.Path, entry.Link) {
			return fmt.Errorf("invalid source link")
		}
		if err := allowed(path.Clean(path.Join(path.Dir(entry.Path), entry.Link)), l); err != nil {
			return err
		}
		if entry.SHA256 != "" || entry.Executable || !safeLink(entry.Path, entry.Link) || int64(len(entry.Link)) != entry.Size {
			return fmt.Errorf("invalid source link")
		}
		return nil
	default:
		return fmt.Errorf("unsupported source kind")
	}
}

func validateRoots(destination, staging string) error {
	for _, root := range []string{destination, staging} {
		if err := realRoot(root); err != nil {
			return err
		}
	}
	destAbs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	stageAbs, err := filepath.Abs(staging)
	if err != nil {
		return err
	}
	if destAbs == stageAbs || strings.HasPrefix(destAbs, stageAbs+string(os.PathSeparator)) || strings.HasPrefix(stageAbs, destAbs+string(os.PathSeparator)) {
		return fmt.Errorf("source roots must be separate")
	}
	return nil
}
func preflightDestination(destination string, previous, next Envelope, old, wanted map[string]Entry) error {
	directories, err := donorDirectories(previous)
	if err != nil {
		return err
	}
	for _, entry := range previous.Entries {
		if err := checkDestination(destination, entry.Path, old); err != nil {
			return err
		}
	}
	for _, entry := range next.Entries {
		if err := checkDestination(destination, entry.Path, old); err != nil {
			return err
		}
		if err := checkDirectoryReplacement(destination, entry.Path, old, wanted, directories); err != nil {
			return err
		}
	}
	return nil
}
func applyEntry(destination string, item preparedEntry) error {
	filename := filepath.Join(destination, item.entry.Path)
	if info, err := os.Lstat(filename); err == nil && info.IsDir() {
		if err := removeEmptyTree(filename); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return err
	}
	if item.entry.Kind == "symlink" {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(item.entry.Link, filename)
	}
	mode := os.FileMode(0644)
	if item.entry.Executable {
		mode = 0755
	}
	return replaceFile(filename, item.data, mode)
}

// Bound implicit directory metadata as well as leaf entries; lookup avoids
// repeatedly scanning every donor entry during directory transitions.
func donorDirectories(previous Envelope) (map[string]bool, error) {
	result := make(map[string]bool)
	for _, entry := range previous.Entries {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			result[parent] = true
			if len(result) > 10000 {
				return nil, fmt.Errorf("source directory inventory exceeds bound")
			}
		}
	}
	return result, nil
}

func validateCompleteAncestors(entries []Entry, complete map[string]bool) error {
	directories := make(map[string]string)
	for _, entry := range entries {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			folded := foldSourcePath(parent)
			if original, seen := directories[folded]; seen && original != parent {
				return fmt.Errorf("source directory case collision")
			}
			directories[folded] = parent
			if len(directories) > 10000 {
				return fmt.Errorf("source directory inventory exceeds bound")
			}
			if complete[folded] {
				return fmt.Errorf("conflicting source paths")
			}
		}
	}
	return nil
}

// Canonicalize the same Unicode simple-fold classes used by strings.EqualFold.
func foldSourcePath(name string) string {
	return strings.Map(func(value rune) rune {
		smallest := value
		for folded := unicode.SimpleFold(value); folded != value; folded = unicode.SimpleFold(folded) {
			if folded < smallest {
				smallest = folded
			}
		}
		return smallest
	}, name)
}
