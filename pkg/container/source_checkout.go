package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/sourcecheckout"
)

// SourceCheckoutReceiver is a controller-supplied admission boundary. It must
// own the initial workspace exclusively and authenticate actual bounded payload
// materialization separately from candidate metadata. Source/WriterAuthority
// come from independent trusted issuers, not this request or a self-seal.
// There is no production receiver/provider or serialized admission route.
type SourceCheckoutReceiver interface {
	MaterializedHandoff(context.Context, string, string, bool) (SourceHandoffInput, error)
}

type SourceHandoffInput struct {
	Candidate sourcecheckout.BaselineCandidate
	Source    sourcecheckout.SourceAuthority
	Writer    sourcecheckout.WriterAuthority
	Limits    sourcecheckout.Limits
}

// PrepareSourceHandoff authenticates a source-only handoff. Compatible output
// transport and real controller grants are independent deployment requirements.
func (e *HostEnvironment) PrepareSourceHandoff(ctx context.Context, destination, source string, candidate sourcecheckout.BaselineCandidate, authority sourcecheckout.SourceAuthority, writer sourcecheckout.WriterAuthority, limits sourcecheckout.Limits) (sourcecheckout.PreparedHandoff, error) {
	if destination != e.Path || filepath.Clean(source) != filepath.Clean(e.Workdir) {
		return sourcecheckout.PreparedHandoff{}, fmt.Errorf("unsupported nested source checkout")
	}
	if err := e.validateSourceOwnedTree(destination); err != nil {
		return sourcecheckout.PreparedHandoff{}, err
	}
	if authority == nil || writer == nil {
		return sourcecheckout.PreparedHandoff{}, fmt.Errorf("missing trusted source cache receiver")
	}
	return sourcecheckout.PrepareHandoff(ctx, candidate, ownedSourceAuthority{authority: authority, source: filepath.Clean(source)}, ownedWriterAuthority{authority: writer, destination: destination}, limits)
}

func (e *HostEnvironment) receiveSourceCheckout(ctx context.Context, destination, source string, useGitIgnore bool) (bool, error) {
	if e.sourceCheckoutAttempted || destination != e.Path || filepath.Clean(source) != filepath.Clean(e.Workdir) {
		return false, nil
	}
	// Git-ignore profiles are unsupported by tracked-source reconciliation.
	// Cold/default, nested and unowned calls retain ordinary CopyDir semantics
	// and never call a receiver or discard any tree.
	if err := e.validateSourceOwnedTree(destination); err != nil {
		return false, nil
	}
	e.sourceCheckoutAttempted = true
	if useGitIgnore {
		return false, nil
	}
	input, err := e.SourceReceiver.MaterializedHandoff(ctx, destination, filepath.Clean(source), useGitIgnore)
	if err == nil {
		err = e.applySourceCheckout(ctx, destination, filepath.Clean(source), input)
	}
	if err == nil {
		return true, nil
	}
	// A rejected materialization or partial merge cannot safely be overwritten
	// additively. Invalidate only this private initial workspace, then let the
	// actual CopyDir perform its existing cold copy. Receiver error text may
	// contain credentials; never log it or the raw authority receipts.
	common.Logger(ctx).Debug("Optional source checkout unavailable; rebuilding owned workspace cold")
	if discardErr := e.discardSourceCheckout(destination); discardErr != nil {
		return false, discardErr
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

func (e *HostEnvironment) applySourceCheckout(ctx context.Context, destination, source string, input SourceHandoffInput) error {
	handoff, err := e.PrepareSourceHandoff(ctx, destination, source, input.Candidate, input.Source, input.Writer, input.Limits)
	if err != nil {
		return err
	}
	metadata, err := handoff.PrepareGit(ctx, e.OwnedRoot)
	if err != nil {
		return err
	}
	if _, err := handoff.Apply(ctx, destination); err != nil {
		return errors.Join(err, metadata.Close())
	}
	err = metadata.Commit()
	return errors.Join(err, metadata.Close())
}

func (e *HostEnvironment) discardSourceCheckout(destination string) error {
	if err := e.validateSourceOwnedTree(destination); err != nil {
		return err
	}
	owner, err := os.OpenRoot(e.OwnedRoot)
	if err != nil {
		return err
	}
	defer owner.Close()
	name := filepath.Base(destination)
	if err := owner.RemoveAll(name); err != nil {
		return err
	}
	if err := owner.Mkdir(name, 0700); err != nil {
		return err
	}
	return e.validateSourceOwnedTree(destination)
}

type ownedSourceAuthority struct {
	authority sourcecheckout.SourceAuthority
	source    string
}

func (authority ownedSourceAuthority) FrozenSource(ctx context.Context) (sourcecheckout.SourceReceipt, error) {
	receipt, err := authority.authority.FrozenSource(ctx)
	if err != nil {
		return receipt, err
	}
	if filepath.Clean(receipt.GitRoot) != authority.source || filepath.Clean(receipt.MaterializedRoot) != authority.source {
		return sourcecheckout.SourceReceipt{}, fmt.Errorf("source authority does not bind actual frozen checkout")
	}
	return receipt, nil
}

type ownedWriterAuthority struct {
	authority   sourcecheckout.WriterAuthority
	destination string
}

func (authority ownedWriterAuthority) ApprovedBaseline(ctx context.Context, namespace, outputs string) (sourcecheckout.BaselineGrant, error) {
	grant, err := authority.authority.ApprovedBaseline(ctx, namespace, outputs)
	if err != nil {
		return grant, err
	}
	if grant.MaterializedRoot != authority.destination {
		return sourcecheckout.BaselineGrant{}, fmt.Errorf("baseline receipt does not bind owned workspace")
	}
	return grant, nil
}

// validateSourceOwnedTree is scoped to initial source admission; it does not
// change Docker-action stage ownership or restoration policy.
func (e *HostEnvironment) validateSourceOwnedTree(destination string) error {
	if e.OwnedRoot == "" || destination != e.Path || filepath.Dir(destination) != e.OwnedRoot {
		return fmt.Errorf("source checkout requires a private owned workspace")
	}
	for _, path := range []string{e.OwnedRoot, destination} {
		info, err := os.Lstat(path)
		if err != nil { return err }
		if !info.IsDir() || info.Mode() & os.ModeSymlink != 0 { return fmt.Errorf("source checkout requires real owned directories") }
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil { return err }
		absolute, err := filepath.Abs(path)
		if err != nil { return err }
		if resolved != absolute { return fmt.Errorf("source checkout owned path has an alias") }
	}
	return nil
}
