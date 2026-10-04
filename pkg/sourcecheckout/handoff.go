package sourcecheckout

import (
	"context"
	"encoding/hex"
	"fmt"
)

// SourceAuthority is supplied by the trusted snapshot controller. Implementations
// must authenticate the receipt independently of workflow/cache-controlled data.
type SourceAuthority interface {
	FrozenSource(context.Context) (SourceReceipt, error)
}
type SourceReceipt struct {
	Identity                                                  FrozenSourceIdentity
	GitRoot, MaterializedRoot, CacheNamespace, OutputIdentity string
}

// WriterAuthority authenticates approved writers, policy and the transported
// payload/materialization. A candidate's self-seal or checkout HEAD is no grant.
// No production provider exists yet; this interface does not approve any writer.
type WriterAuthority interface {
	ApprovedBaseline(context.Context, string, string) (BaselineGrant, error)
}
type BaselineGrant struct {
	Source                                                          FrozenSourceIdentity
	CacheNamespace, OutputIdentity, PayloadSHA256, MaterializedRoot string
	PolicyCommit, WorkflowCommit                                    string
	RunID, Attempt, JobID                                           int64
}

// BaselineCandidate is receiver data. PayloadSHA256 must be computed from the
// actual bounded transport payload, never taken from an archive's own marker.
type BaselineCandidate struct {
	Envelope      Envelope
	PayloadSHA256 string
}

// PreparedHandoff carries independently verified inventories, but is not an
// apply operation. Authoritative .git and compatible output transport are still
// required before a production warm checkout can consume it.
type PreparedHandoff struct {
	baseline, requested VerifiedCheckout
	grant               BaselineGrant
	ready               bool
}

func (handoff PreparedHandoff) Ready() bool         { return handoff.ready }
func (handoff PreparedHandoff) Baseline() Envelope  { return handoff.baseline.Envelope() }
func (handoff PreparedHandoff) Requested() Envelope { return handoff.requested.Envelope() }

// PrepareHandoff is read-only and fail closed. Errors mean ordinary checkout
// fallback; a zero handoff can never authorize reconciliation.
func PrepareHandoff(ctx context.Context, candidate BaselineCandidate, source SourceAuthority, writer WriterAuthority, limits Limits) (PreparedHandoff, error) {
	if source == nil || writer == nil {
		return PreparedHandoff{}, fmt.Errorf("missing source or approved writer authority")
	}
	receipt, err := source.FrozenSource(ctx)
	if err != nil {
		return PreparedHandoff{}, err
	}
	if receipt.CacheNamespace == "" || len(receipt.CacheNamespace) > 256 || !digestString(receipt.OutputIdentity) {
		return PreparedHandoff{}, fmt.Errorf("unsupported source cache identity")
	}
	grant, err := writer.ApprovedBaseline(ctx, receipt.CacheNamespace, receipt.OutputIdentity)
	if err != nil {
		return PreparedHandoff{}, err
	}
	if err := validateGrant(grant, receipt, candidate); err != nil {
		return PreparedHandoff{}, err
	}
	next, err := ReadFrozenCheckout(receipt.GitRoot, receipt.MaterializedRoot, receipt.Identity, limits)
	if err != nil {
		return PreparedHandoff{}, err
	}
	donor, err := ReadFrozenCheckout(receipt.GitRoot, grant.MaterializedRoot, grant.Source, limits)
	if err != nil {
		return PreparedHandoff{}, err
	}
	donor.envelope.Binding.OutputIdentity = grant.OutputIdentity
	donor.envelope.ContentID = envelopeID(donor.envelope)
	if err := validateEnvelope(candidate.Envelope, limits); err != nil {
		return PreparedHandoff{}, err
	}
	if candidate.Envelope.ContentID != donor.envelope.ContentID {
		return PreparedHandoff{}, fmt.Errorf("candidate source differs from authoritative Git inventory")
	}
	next.envelope.Binding.OutputIdentity = receipt.OutputIdentity
	next.envelope.ContentID = envelopeID(next.envelope)
	return PreparedHandoff{baseline: donor, requested: next, grant: grant, ready: true}, nil
}
func validateGrant(grant BaselineGrant, receipt SourceReceipt, candidate BaselineCandidate) error {
	if grant.CacheNamespace != receipt.CacheNamespace || grant.OutputIdentity != receipt.OutputIdentity {
		return fmt.Errorf("cache family or compatible output identity mismatch")
	}
	if !digestString(grant.PayloadSHA256) || candidate.PayloadSHA256 != grant.PayloadSHA256 {
		return fmt.Errorf("transport payload identity mismatch")
	}
	if !gitCommitString(grant.PolicyCommit) || !gitCommitString(grant.WorkflowCommit) || grant.RunID <= 0 || grant.Attempt <= 0 || grant.JobID <= 0 {
		return fmt.Errorf("incomplete approved writer grant")
	}
	if grant.MaterializedRoot == "" || grant.MaterializedRoot == receipt.MaterializedRoot || grant.MaterializedRoot == receipt.GitRoot {
		return fmt.Errorf("invalid baseline materialization")
	}
	return validateFrozenIdentity(grant.Source)
}
func digestString(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func gitCommitString(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
