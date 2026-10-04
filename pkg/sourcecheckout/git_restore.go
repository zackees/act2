package sourcecheckout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
)

// PreparedGit is separate from source reconciliation. Only independently
// verified requested Git objects/refs are restored; no cached Git metadata,
// source config, credential-bearing remotes, attributes, or hooks are copied.
// The caller owns parent/destination exclusively until Close.
type PreparedGit struct {
	parent      *os.Root
	stage       string
	destination string
}

func (handoff PreparedHandoff) PrepareGit(ctx context.Context, parent string) (*PreparedGit, error) {
	if !handoff.ready || filepath.Dir(handoff.baseline.root) != parent {
		return nil, fmt.Errorf("Git restoration requires the approved owned workspace parent")
	}
	if err := realRoot(parent); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(parent, ".source-git-")
	if err != nil {
		return nil, err
	}
	owner, err := os.OpenRoot(parent)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(stage))
	}
	prepared := &PreparedGit{parent: owner, stage: filepath.Base(stage), destination: filepath.Base(handoff.baseline.root)}
	if err := handoff.prepareGitFiles(ctx, stage); err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	// Re-read copied objects with the requested independent identity before any
	// destination mutation. The source tree stays in its frozen original root.
	if _, err := ReadFrozenCheckout(stage, handoff.requested.root, handoff.requested.identity, handoff.limits); err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	return prepared, nil
}

func (handoff PreparedHandoff) prepareGitFiles(ctx context.Context, stage string) error {
	source := filepath.Join(handoff.requested.gitRoot, ".git")
	if err := boundedGitMetadata(source); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(stage, ".git"), 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	destination, err := os.OpenRoot(filepath.Join(stage, ".git"))
	if err != nil {
		return err
	}
	defer destination.Close()
	if err := copyAuthoritativeGit(ctx, root, destination); err != nil {
		return err
	}
	if err := destination.WriteFile("HEAD", []byte(handoff.requested.identity.CheckoutCommit+"\n"), 0600); err != nil {
		return err
	}
	// Never preserve source hooksPath, filters, includes, ssh commands or remotes.
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tfilemode = true\n"
	if err := destination.WriteFile("config", []byte(config), 0600); err != nil {
		return err
	}
	return handoff.writeRequestedIndex(destination)
}

func copyAuthoritativeGit(ctx context.Context, source, destination *os.Root) error {
	var total int64
	count := 0
	return fs.WalkDir(source.FS(), ".", func(name string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		selected := name == "objects" || strings.HasPrefix(name, "objects/") || name == "refs" || strings.HasPrefix(name, "refs/") || name == "packed-refs"
		if !selected {
			if item.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		count++
		if count > 10000 {
			return fmt.Errorf("Git restoration entry bound exceeded")
		}
		if item.IsDir() {
			return destination.MkdirAll(name, 0700)
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 128<<20-total {
			return fmt.Errorf("Git restoration payload bound exceeded")
		}
		written, err := copyGitFile(ctx, source, destination, name, info.Size())
		if err != nil {
			return err
		}
		total += written
		return nil
	})
}

func copyGitFile(ctx context.Context, source, destination *os.Root, name string, size int64) (int64, error) {
	input, err := source.Open(name)
	if err != nil {
		return 0, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return 0, fmt.Errorf("Git restoration file changed")
	}
	output, err := destination.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(output, io.LimitReader(gitRestoreReader{ctx: ctx, reader: input}, size+1))
	closeErr := output.Close()
	if written != size {
		copyErr = errors.Join(copyErr, fmt.Errorf("Git restoration size changed"))
	}
	return written, errors.Join(copyErr, closeErr)
}

func (handoff PreparedHandoff) writeRequestedIndex(destination *os.Root) error {
	items, err := prepare(handoff.requested.root, handoff.requested.envelope, handoff.limits)
	if err != nil {
		return err
	}
	requested := &index.Index{Version: 2}
	for _, item := range items {
		mode := filemode.Regular
		data := item.data
		if item.entry.Kind == "symlink" {
			mode, data = filemode.Symlink, []byte(item.entry.Link)
		} else if item.entry.Executable {
			mode = filemode.Executable
		}
		// Index stats are deliberately invalidated; source mtimes are never
		// transplanted or forged. The content/mode identity comes from Git.
		requested.Entries = append(requested.Entries, &index.Entry{
			Name: item.entry.Path, Hash: plumbing.ComputeHash(plumbing.BlobObject, data),
			Mode: mode, Size: uint32(len(data)), CreatedAt: time.Unix(0, 0), ModifiedAt: time.Unix(0, 0),
		})
	}
	file, err := destination.OpenFile("index", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return errors.Join(index.NewEncoder(file).Encode(requested), file.Close())
}

// Commit replaces only .git after source application succeeds. A failure is not
// a usable checkout: the caller must discard the owned workspace and go cold.
func (prepared *PreparedGit) Commit() error {
	if prepared == nil || prepared.parent == nil {
		return fmt.Errorf("Git restoration was not prepared")
	}
	target := filepath.Join(prepared.destination, ".git")
	backup := filepath.Join(prepared.stage, "previous")
	if err := prepared.parent.Rename(target, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	return prepared.parent.Rename(filepath.Join(prepared.stage, ".git"), target)
}

func (prepared *PreparedGit) Close() error {
	if prepared == nil || prepared.parent == nil {
		return nil
	}
	err := prepared.parent.RemoveAll(prepared.stage)
	err = errors.Join(err, prepared.parent.Close())
	prepared.parent = nil
	return err
}

// Cancellation bounds each read as well as archive traversal.
type gitRestoreReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader gitRestoreReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
