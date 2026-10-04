// Package sourcecheckout reconciles explicit source inventories into owned writable trees.
package sourcecheckout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits bound both metadata and source bytes before materialization.
type Limits struct {
	Files                 int
	FileBytes, TotalBytes int64
	Protected             []string
}

func DefaultLimits() Limits {
	return Limits{Files: 10000, FileBytes: 64 << 20, TotalBytes: 256 << 20, Protected: []string{"target"}}
}

type Entry struct {
	Path       string
	Kind       string
	Executable bool
	Size       int64
	SHA256     string
	Link       string
}

// Binding must come from trusted checkout/build policy. ContentID proves the
// envelope's own identity, not the authenticity of these provenance claims.
type Binding struct{ SourceCommit, GitTree, OutputIdentity string }
type Envelope struct {
	Version   int
	Entries   []Entry
	ContentID string
	Binding   Binding
}
type Result struct{ Unchanged, Written, Deleted int }

func validLimits(l Limits) error {
	if l.Files <= 0 || l.Files > 10000 || l.FileBytes <= 0 || l.FileBytes > 64<<20 || l.TotalBytes <= 0 || l.TotalBytes > 256<<20 || len(l.Protected) > 100 {
		return fmt.Errorf("invalid source limits")
	}
	for _, p := range l.Protected {
		if len(p) > 4096 || p == "" || path.Clean(p) != p || path.IsAbs(p) || !utf8.ValidString(p) {
			return fmt.Errorf("invalid protected source prefix")
		}
	}
	return nil
}
func allowed(name string, l Limits) error {
	if !utf8.ValidString(name) || len(name) > 4096 || name == "" || path.Clean(name) != name || path.IsAbs(name) || strings.ContainsAny(name, "\\:\x00") {
		return fmt.Errorf("unsafe source path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.EqualFold(strings.TrimRight(part, " ."), ".git") {
			return fmt.Errorf("protected source path %q", name)
		}
	}
	for _, p := range append([]string{"target"}, l.Protected...) {
		if strings.EqualFold(name, p) || strings.HasPrefix(strings.ToLower(name), strings.ToLower(p)+"/") || strings.HasPrefix(strings.ToLower(p), strings.ToLower(name)+"/") {
			return fmt.Errorf("protected source path %q", name)
		}
	}
	return nil
}
func realRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return fmt.Errorf("source root ancestors must be real directories")
	}
	return nil
}
func realParents(root, name string) error {
	current := root
	components := strings.Split(name, "/")
	for _, part := range components[:len(components)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe source ancestor %q", name)
		}
	}
	return nil
}
func safeLink(name, target string) bool {
	if !utf8.ValidString(target) || target == "" || len(target) > 4096 || path.IsAbs(target) || strings.ContainsAny(target, "\\\x00") {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return false
	}
	for _, part := range strings.Split(resolved, "/") {
		if strings.EqualFold(strings.TrimRight(part, " ."), ".git") {
			return false
		}
	}
	return true
}
func readFile(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("source exceeds regular file bound")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != info.Size() {
		return nil, fmt.Errorf("source changed or exceeded bound")
	}
	return data, nil
}
func envelopeID(e Envelope) string {
	data, _ := json.Marshal(struct {
		Version int
		Entries []Entry
		Binding Binding
	}{e.Version, e.Entries, e.Binding})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Bind records externally verified Git/build identity and reseals metadata.
func (e Envelope) Bind(binding Binding) (Envelope, error) {
	if err := validateEnvelope(e, DefaultLimits()); err != nil {
		return Envelope{}, err
	}
	if err := validBinding(binding, false); err != nil {
		return Envelope{}, err
	}
	e.Binding = binding
	e.ContentID = envelopeID(e)
	return e, nil
}
func validBinding(binding Binding, allowEmpty bool) error {
	if allowEmpty && binding == (Binding{}) {
		return nil
	}
	for _, id := range []string{binding.SourceCommit, binding.GitTree} {
		if len(id) != 40 && len(id) != 64 {
			return fmt.Errorf("full Git object ID required")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return err
		}
	}
	if len(binding.OutputIdentity) > 4096 {
		return fmt.Errorf("output identity exceeds bound")
	}
	return nil
}
func BuildEnvelope(root string, paths []string, l Limits) (Envelope, error) {
	if err := validLimits(l); err != nil {
		return Envelope{}, err
	}
	if len(paths) > l.Files {
		return Envelope{}, fmt.Errorf("source inventory exceeds bound")
	}
	if err := realRoot(root); err != nil {
		return Envelope{}, err
	}
	result := Envelope{Version: 1}
	total := int64(0)
	names := append([]string(nil), paths...)
	sort.Strings(names)
	for i, name := range names {
		if i > 0 && names[i-1] == name {
			return Envelope{}, fmt.Errorf("duplicate source path")
		}
		if err := allowed(name, l); err != nil {
			return Envelope{}, err
		}
		if err := realParents(root, name); err != nil {
			return Envelope{}, err
		}
		filename := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(filename)
		if err != nil {
			return Envelope{}, err
		}
		entry := Entry{Path: name, Size: info.Size(), Executable: info.Mode()&0111 != 0}
		if entry.Size < 0 || entry.Size > l.FileBytes || entry.Size > l.TotalBytes-total {
			return Envelope{}, fmt.Errorf("source byte bound exceeded")
		}
		total += entry.Size
		if info.Mode().IsRegular() {
			data, err := readFile(filename, l.FileBytes)
			if err != nil {
				return Envelope{}, err
			}
			sum := sha256.Sum256(data)
			entry.Kind = "file"
			entry.SHA256 = hex.EncodeToString(sum[:])
		} else if info.Mode()&os.ModeSymlink != 0 {
			entry.Kind = "symlink"
			entry.Executable = false
			entry.Link, err = os.Readlink(filename)
			if err != nil {
				return Envelope{}, err
			}
			if !safeLink(name, entry.Link) {
				return Envelope{}, fmt.Errorf("unsafe source symlink")
			}
			if err := allowed(path.Clean(path.Join(path.Dir(name), entry.Link)), l); err != nil {
				return Envelope{}, err
			}
		} else {
			return Envelope{}, fmt.Errorf("unsupported source type")
		}
		result.Entries = append(result.Entries, entry)
	}
	result.ContentID = envelopeID(result)
	if err := validateEnvelope(result, l); err != nil {
		return Envelope{}, err
	}
	return result, nil
}
