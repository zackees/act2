package sourcecheckout

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// FrozenSourceIdentity comes from the trusted snapshot controller, not cache
// metadata or HEAD. CheckoutCommit is the synthetic commit for dirty snapshots.
type FrozenSourceIdentity struct {
	OriginalCommit string
	CheckoutCommit string
	GitTree        string
	TreeDigest     string
	Dirty          bool
}

// VerifiedCheckout can only be constructed after independent Git verification.
// Its Git authority and materialized source roots are deliberately separate.
type VerifiedCheckout struct {
	root     string
	gitRoot  string
	envelope Envelope
	identity FrozenSourceIdentity
	verified bool
}

type gitInventoryReader struct {
	repository                 *git.Repository
	limits                     Limits
	metadataBytes, sourceBytes int64
	directories                int
	entries                    []Entry
}
type gitTreePath struct {
	hash   plumbing.Hash
	prefix string
}

func validateFrozenIdentity(identity FrozenSourceIdentity) error {
	for _, value := range []string{identity.OriginalCommit, identity.CheckoutCommit, identity.GitTree} {
		if len(value) != 40 {
			return fmt.Errorf("unsupported Git object format or missing frozen identity")
		}
		if _, err := hex.DecodeString(value); err != nil {
			return err
		}
	}
	if len(identity.TreeDigest) != 64 {
		return fmt.Errorf("missing frozen tree digest")
	}
	if _, err := hex.DecodeString(identity.TreeDigest); err != nil {
		return err
	}
	if !identity.Dirty && identity.CheckoutCommit != identity.OriginalCommit {
		return fmt.Errorf("clean frozen source must use its original commit")
	}
	return nil
}

// ReadFrozenCheckout verifies explicit controller-provided identities against
// read-only Git objects and actual source bytes/modes. It executes no Git tool,
// hooks, credentials or network request, and never opens the baseline's .git.
func ReadFrozenCheckout(gitRoot, materializedRoot string, expected FrozenSourceIdentity, l Limits) (VerifiedCheckout, error) {
	if err := validLimits(l); err != nil {
		return VerifiedCheckout{}, err
	}
	if err := validateFrozenIdentity(expected); err != nil {
		return VerifiedCheckout{}, err
	}
	for _, root := range []string{gitRoot, materializedRoot} {
		if err := realRoot(root); err != nil {
			return VerifiedCheckout{}, err
		}
	}
	if err := realRoot(filepath.Join(gitRoot, ".git")); err != nil {
		return VerifiedCheckout{}, fmt.Errorf("frozen Git metadata must be an owned real directory: %w", err)
	}
	if err := boundedGitMetadata(filepath.Join(gitRoot, ".git")); err != nil {
		return VerifiedCheckout{}, err
	}
	repository, err := git.PlainOpen(gitRoot)
	if err != nil {
		return VerifiedCheckout{}, err
	}
	reader := gitInventoryReader{repository: repository, limits: l}
	envelope, err := reader.inventory(expected)
	if err != nil {
		return VerifiedCheckout{}, err
	}
	if _, err := prepare(materializedRoot, envelope, l); err != nil {
		return VerifiedCheckout{}, err
	}
	if err := validateLinks(materializedRoot, "", Envelope{}, envelope, l); err != nil {
		return VerifiedCheckout{}, err
	}
	return VerifiedCheckout{root: materializedRoot, gitRoot: gitRoot, envelope: envelope, identity: expected, verified: true}, nil
}

func (reader *gitInventoryReader) encoded(hash plumbing.Hash, kind plumbing.ObjectType, limit int64) (plumbing.EncodedObject, []byte, error) {
	encoded, err := reader.repository.Storer.EncodedObject(kind, hash)
	if err != nil {
		return nil, nil, err
	}
	if encoded.Type() != kind || encoded.Size() < 0 || encoded.Size() > limit {
		return nil, nil, fmt.Errorf("Git object exceeds bound or has wrong type")
	}
	stream, err := encoded.Reader()
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) != encoded.Size() || plumbing.ComputeHash(kind, data) != hash {
		return nil, nil, fmt.Errorf("Git object content identity mismatch")
	}
	return encoded, data, nil
}
func (reader *gitInventoryReader) metadata(hash plumbing.Hash, kind plumbing.ObjectType) (plumbing.EncodedObject, error) {
	limit := int64(1 << 20)
	remaining := int64(16<<20) - reader.metadataBytes
	if remaining < limit {
		limit = remaining
	}
	encoded, _, err := reader.encoded(hash, kind, limit)
	if err != nil {
		return nil, err
	}
	if encoded.Size() > 16<<20-reader.metadataBytes {
		return nil, fmt.Errorf("Git metadata exceeds total bound")
	}
	reader.metadataBytes += encoded.Size()
	return encoded, nil
}
func (reader *gitInventoryReader) inventory(expected FrozenSourceIdentity) (Envelope, error) {
	encoded, err := reader.metadata(plumbing.NewHash(expected.CheckoutCommit), plumbing.CommitObject)
	if err != nil {
		return Envelope{}, err
	}
	commit, err := object.DecodeCommit(reader.repository.Storer, encoded)
	if err != nil {
		return Envelope{}, err
	}
	if commit.TreeHash.String() != expected.GitTree {
		return Envelope{}, fmt.Errorf("frozen Git tree identity mismatch")
	}
	if expected.Dirty && (len(commit.ParentHashes) != 1 || commit.ParentHashes[0].String() != expected.OriginalCommit) {
		return Envelope{}, fmt.Errorf("synthetic checkout does not descend from original commit")
	}
	reader.directories = 1
	queue := []gitTreePath{{hash: commit.TreeHash}}
	for len(queue) > 0 {
		treePath := queue[0]
		queue = queue[1:]
		children, err := reader.tree(treePath)
		if err != nil {
			return Envelope{}, err
		}
		queue = append(queue, children...)
	}
	sort.Slice(reader.entries, func(i, j int) bool { return reader.entries[i].Path < reader.entries[j].Path })
	envelope := Envelope{Version: 1, Entries: reader.entries, Binding: Binding{SourceCommit: expected.CheckoutCommit, GitTree: expected.GitTree}}
	envelope.ContentID = envelopeID(envelope)
	if err := validateEnvelope(envelope, reader.limits); err != nil {
		return Envelope{}, err
	}
	if frozenTreeDigest(envelope.Entries) != expected.TreeDigest {
		return Envelope{}, fmt.Errorf("frozen source digest mismatch")
	}
	return envelope, nil
}
func (reader *gitInventoryReader) tree(location gitTreePath) ([]gitTreePath, error) {
	encoded, err := reader.metadata(location.hash, plumbing.TreeObject)
	if err != nil {
		return nil, err
	}
	tree, err := object.DecodeTree(reader.repository.Storer, encoded)
	if err != nil {
		return nil, err
	}
	var children []gitTreePath
	for _, item := range tree.Entries {
		name := path.Join(location.prefix, item.Name)
		if item.Name == "" || strings.ContainsAny(item.Name, "/\\") || item.Name == "." || item.Name == ".." {
			return nil, fmt.Errorf("invalid Git tree entry name")
		}
		if err := allowed(name, reader.limits); err != nil {
			return nil, err
		}
		if item.Mode == filemode.Dir {
			if reader.directories >= 10000 {
				return nil, fmt.Errorf("Git directory inventory exceeds bound")
			}
			reader.directories++
			children = append(children, gitTreePath{hash: item.Hash, prefix: name})
			continue
		}
		if len(reader.entries) >= reader.limits.Files {
			return nil, fmt.Errorf("Git source inventory exceeds bound")
		}
		entry, err := reader.blob(name, item)
		if err != nil {
			return nil, err
		}
		reader.entries = append(reader.entries, entry)
	}
	return children, nil
}
func (reader *gitInventoryReader) blob(name string, item object.TreeEntry) (Entry, error) {
	if item.Mode != filemode.Regular && item.Mode != filemode.Executable && item.Mode != filemode.Symlink {
		return Entry{}, fmt.Errorf("unsupported Git source mode (including submodules)")
	}
	remaining := reader.limits.TotalBytes - reader.sourceBytes
	limit := reader.limits.FileBytes
	if remaining < limit {
		limit = remaining
	}
	_, data, err := reader.encoded(item.Hash, plumbing.BlobObject, limit)
	if err != nil {
		return Entry{}, err
	}
	reader.sourceBytes += int64(len(data))
	if bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\n")) {
		return Entry{}, fmt.Errorf("Git LFS source requires a separate checkout contract")
	}
	entry := Entry{Path: name, Size: int64(len(data)), Executable: item.Mode == filemode.Executable}
	if item.Mode == filemode.Symlink {
		entry.Kind = "symlink"
		entry.Link = string(data)
	} else {
		entry.Kind = "file"
		sum := sha256.Sum256(data)
		entry.SHA256 = hex.EncodeToString(sum[:])
	}
	return entry, nil
}

// Match Bosn's sorted relative PathBuf inventory digest, not JSON or commit time.
func frozenTreeDigest(entries []Entry) string {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return componentPathLess(sorted[i].Path, sorted[j].Path) })
	digest := sha256.New()
	for _, entry := range sorted {
		_, _ = io.WriteString(digest, entry.Path+"\x00")
		if entry.Kind == "symlink" {
			_, _ = io.WriteString(digest, "link\x00"+entry.Link)
		} else {
			kind := "f\x00"
			if entry.Executable {
				kind = "x\x00"
			}
			_, _ = io.WriteString(digest, kind+entry.SHA256)
		}
		_, _ = io.WriteString(digest, "\n")
	}
	return hex.EncodeToString(digest.Sum(nil))
}
func componentPathLess(left, right string) bool {
	a, b := strings.Split(left, "/"), strings.Split(right, "/")
	for index := 0; index < len(a) && index < len(b); index++ {
		if a[index] != b[index] {
			return a[index] < b[index]
		}
	}
	return len(a) < len(b)
}

// Envelope returns detached data, not a writer-trust grant.
func (checkout VerifiedCheckout) Envelope() Envelope {
	result := checkout.envelope
	result.Entries = append([]Entry(nil), result.Entries...)
	return result
}

func boundedGitMetadata(root string) error {
	files := 0
	bytes := int64(0)
	inflated := int64(0)
	return filepath.WalkDir(root, func(name string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		files++
		if files > 10000 {
			return fmt.Errorf("Git metadata entry bound exceeded")
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 128<<20-bytes {
			return fmt.Errorf("Git metadata payload bound exceeded")
		}
		bytes += info.Size()
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if strings.HasPrefix(relative, "objects/pack/") {
			return fmt.Errorf("packed Git object stores require a bounded transport decoder")
		}
		if strings.HasPrefix(relative, "objects/") && len(relative) == 49 && relative[10] == '/' {
			size, err := preflightLooseObject(name)
			if err != nil {
				return err
			}
			if size > 256<<20-inflated {
				return fmt.Errorf("inflated Git object store exceeds bound")
			}
			inflated += size
		}
		if relative == "objects/info/alternates" || relative == "commondir" {
			return fmt.Errorf("Git alternate object stores are unsupported")
		}
		return nil
	})
}

// Validate the compressed object header before Go-git's memory-backed decoder.
// Packed stores are unsupported until the trusted receiver supplies a bounded
// object decoder; they fall back to ordinary checkout.
func preflightLooseObject(name string) (int64, error) {
	file, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	stream, err := zlib.NewReader(file)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	reader := bufio.NewReaderSize(stream, 256)
	header, err := reader.ReadSlice(0)
	if err != nil || len(header) > 128 {
		return 0, fmt.Errorf("invalid bounded Git object header")
	}
	parts := strings.Split(string(header[:len(header)-1]), " ")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid Git object header")
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || size < 0 || size > 64<<20 {
		return 0, fmt.Errorf("inflated Git object exceeds bound")
	}
	if parts[0] != "blob" && parts[0] != "tree" && parts[0] != "commit" && parts[0] != "tag" {
		return 0, fmt.Errorf("unsupported Git object type")
	}
	if parts[0] != "blob" && size > 1<<20 {
		return 0, fmt.Errorf("Git metadata object exceeds bound")
	}
	actual, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || actual != size {
		return 0, fmt.Errorf("Git object payload size mismatch")
	}
	return size, nil
}
