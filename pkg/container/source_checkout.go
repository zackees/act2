package container

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nektos/act/pkg/sourcecheckout"
)

// PrepareSourceHandoff validates an optional source-cache handoff without
// applying it. CopyDir remains ordinary checkout until source/output transport
// and authoritative .git replacement have been integrated and measured.
func (e *HostEnvironment) PrepareSourceHandoff(ctx context.Context, destination, source string, candidate sourcecheckout.BaselineCandidate, authority sourcecheckout.SourceAuthority, writer sourcecheckout.WriterAuthority, limits sourcecheckout.Limits) (sourcecheckout.PreparedHandoff, error) {
	if destination != e.Path || filepath.Clean(source) != filepath.Clean(e.Workdir) {
		return sourcecheckout.PreparedHandoff{}, fmt.Errorf("unsupported nested source checkout")
	}
	if err := e.validateOwnedTree(destination); err != nil {
		return sourcecheckout.PreparedHandoff{}, err
	}
	if authority == nil || writer == nil {
		return sourcecheckout.PreparedHandoff{}, fmt.Errorf("missing trusted source cache receiver")
	}
	return sourcecheckout.PrepareHandoff(ctx, candidate, ownedSourceAuthority{authority: authority, source: filepath.Clean(source)}, ownedWriterAuthority{authority: writer, destination: destination}, limits)
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
