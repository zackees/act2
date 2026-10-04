package git

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// maxImmutableTagChain bounds tag peeling so a cyclic or absurd tag chain fails
// closed instead of looping.
const maxImmutableTagChain = 8

func isImmutableGitPin(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	_, err := hex.DecodeString(ref)
	return err == nil
}

// Immutable acquisition stays inside the caller's cancellation-aware global
// clone gate. Cache names and existing commit filenames are not evidence of a
// hit: verify the origin and complete commit/tree/blob contents before reuse.
func acquireImmutableGitPin(ctx context.Context, input NewGitCloneExecutorInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, existedErr := gogit.PlainOpen(input.Dir)
	offlineExisting := input.OfflineMode && existedErr == nil
	repo, err := immutableGitRepository(input)
	if err != nil {
		return err
	}
	pin := plumbing.NewHash(input.Ref)
	commit, err := resolveImmutablePinCommit(repo, pin)
	if errors.Is(err, plumbing.ErrObjectNotFound) && !offlineExisting {
		err = fetchImmutableGitPin(ctx, repo, input, pin)
		if err != nil {
			return err
		}
		// A nil fetch error is not evidence the pin arrived: upload-pack can
		// accept a want it does not satisfy and still answer successfully.
		// Re-resolve, and report a diagnosable failure rather than the bare
		// plumbing error a later verification would surface.
		commit, err = resolveImmutablePinCommit(repo, pin)
	}
	if err != nil {
		return err
	}
	err = verifyImmutableGitCheckout(ctx, repo, commit)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}
	if err = worktree.Checkout(&gogit.CheckoutOptions{Hash: commit, Force: true}); err != nil {
		return err
	}
	return worktree.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: commit})
}

// resolveImmutablePinCommit returns the commit a 40-hex pin names. A pin may be
// an annotated tag object rather than a commit; git peels those, so act2 must
// too or an ordinary action pin can never be acquired.
func resolveImmutablePinCommit(repo *gogit.Repository, pin plumbing.Hash) (plumbing.Hash, error) {
	// Peel defensively: a tag may target another tag.
	for range maxImmutableTagChain {
		if _, err := repo.Storer.EncodedObject(plumbing.CommitObject, pin); err == nil {
			return pin, nil
		} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return plumbing.ZeroHash, err
		}
		encoded, err := repo.Storer.EncodedObject(plumbing.TagObject, pin)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		var tag object.Tag
		if err = tag.Decode(encoded); err != nil {
			return plumbing.ZeroHash, err
		}
		pin = tag.Target
	}
	return plumbing.ZeroHash, fmt.Errorf("immutable Git pin %s exceeds the %d tag chain limit", pin, maxImmutableTagChain)
}

func immutableGitRepository(input NewGitCloneExecutorInput) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpen(input.Dir)
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		repo, err = gogit.PlainInit(input.Dir, false)
		if err != nil {
			return nil, err
		}
		if _, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{input.URL}}); err != nil {
			return nil, err
		}
		if err = os.Chmod(input.Dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, err
	}
	urls := remote.Config().URLs
	if len(urls) != 1 || urls[0] != input.URL {
		return nil, errors.New("immutable Git checkout origin does not match requested source")
	}
	return repo, nil
}

func fetchImmutableGitPin(ctx context.Context, repo *gogit.Repository, input NewGitCloneExecutorInput, pin plumbing.Hash) error {
	options, _ := gitOptions(input.Token)
	options.Depth = 1
	options.Tags = gogit.NoTags
	options.RefSpecs = []config.RefSpec{config.RefSpec(pin.String() + ":refs/act2/pins/" + pin.String())}
	err := repo.FetchContext(ctx, &options)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return nil
	}
	if err == nil || !unsupportedImmutableGitPin(err, pin) {
		return err
	}
	// Only explicit protocol refusal permits one broad fallback. Authentication,
	// cancellation, I/O failures and malformed objects do not authorize it.
	legacy, _ := gitOptions(input.Token)
	err = repo.FetchContext(ctx, &legacy)
	if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

func unsupportedImmutableGitPin(err error, pin plumbing.Hash) bool {
	if errors.Is(err, gogit.ErrExactSHA1NotSupported) {
		return true
	}
	var refusal *pktline.ErrorLine
	if errors.As(err, &refusal) {
		return refusal.Text == "upload-pack: not our ref" || refusal.Text == "upload-pack: not our ref "+pin.String()
	}
	// go-git v5.16.5's transport/internal/common.go wraps this protocol error
	// with %s, erasing ErrorLine's type. Accept only its complete, exact refusal
	// rendering; never a substring, arbitrary HTTP error or malformed pack.
	const refusalMessage = "error decoding upload-pack response: upload-pack: not our ref"
	return err.Error() == refusalMessage || err.Error() == refusalMessage+" "+pin.String()
}

func verifyImmutableGitCheckout(ctx context.Context, repo *gogit.Repository, pin plumbing.Hash) error {
	if err := verifyImmutableGitObject(ctx, repo, plumbing.CommitObject, pin); err != nil {
		return err
	}
	commit, err := repo.CommitObject(pin)
	if err != nil {
		return err
	}
	return verifyImmutableGitTree(ctx, repo, commit.TreeHash, make(map[plumbing.Hash]bool))
}

func verifyImmutableGitTree(ctx context.Context, repo *gogit.Repository, hash plumbing.Hash, visited map[plumbing.Hash]bool) error {
	if visited[hash] {
		return nil
	}
	visited[hash] = true
	if err := verifyImmutableGitObject(ctx, repo, plumbing.TreeObject, hash); err != nil {
		return err
	}
	tree, err := repo.TreeObject(hash)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		switch entry.Mode {
		case filemode.Dir:
			err = verifyImmutableGitTree(ctx, repo, entry.Hash, visited)
		case filemode.Submodule:
			// Preserve gitlinks without acquiring the external submodule history,
			// matching the ordinary clone executor's existing checkout semantics.
			continue
		case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
			err = verifyImmutableGitObject(ctx, repo, plumbing.BlobObject, entry.Hash)
		default:
			return errors.New("invalid mode in immutable Git tree")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyImmutableGitObject(ctx context.Context, repo *gogit.Repository, kind plumbing.ObjectType, hash plumbing.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := repo.Storer.EncodedObject(kind, hash)
	if err != nil {
		return err
	}
	reader, err := encoded.Reader()
	if err != nil {
		return err
	}
	defer reader.Close()
	hasher := plumbing.NewHasher(kind, encoded.Size())
	if _, err = io.Copy(&hasher, immutableGitReader{ctx: ctx, reader: reader}); err != nil {
		return err
	}
	if hasher.Sum() != hash {
		return fmt.Errorf("immutable Git %s object checksum mismatch", kind)
	}
	return ctx.Err()
}

type immutableGitReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader immutableGitReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
