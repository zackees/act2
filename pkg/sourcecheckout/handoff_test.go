package sourcecheckout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixtureSourceAuthority struct{ receipt SourceReceipt }

func (authority fixtureSourceAuthority) FrozenSource(context.Context) (SourceReceipt, error) {
	return authority.receipt, nil
}

type fixtureWriterAuthority struct {
	grant  BaselineGrant
	denied bool
}

func (authority fixtureWriterAuthority) ApprovedBaseline(_ context.Context, namespace, output string) (BaselineGrant, error) {
	if authority.denied || namespace != authority.grant.CacheNamespace || output != authority.grant.OutputIdentity {
		return BaselineGrant{}, fmt.Errorf("unapproved cache writer")
	}
	return authority.grant, nil
}
func TestHandoffRequiresIndependentWriterAndTransport(t *testing.T) {
	for _, scenario := range []string{"ready", "unapproved", "transport", "output", "source-seal", "writer-receipt", "missing-authority", "poisoned-baseline"} {
		t.Run(scenario, func(t *testing.T) {
			root, repo, original := gitFixture(t, "old")
			baseline := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(baseline, "source.rs"), []byte("old"), 0600))
			donor, err := ReadFrozenCheckout(root, baseline, original, DefaultLimits())
			require.NoError(t, err)
			output := strings.Repeat("a", 64)
			envelope, err := donor.Envelope().Bind(Binding{SourceCommit: original.CheckoutCommit, GitTree: original.GitTree, OutputIdentity: output})
			require.NoError(t, err)
			requested := commitFixture(t, root, repo, "new")
			requested.Dirty = true
			requested.OriginalCommit = original.OriginalCommit
			receipt := SourceReceipt{Identity: requested, GitRoot: root, MaterializedRoot: root, CacheNamespace: "repository-source-v1", OutputIdentity: output}
			grant := BaselineGrant{Source: original, CacheNamespace: receipt.CacheNamespace, OutputIdentity: output, PayloadSHA256: strings.Repeat("b", 64), MaterializedRoot: baseline, PolicyCommit: original.OriginalCommit, WorkflowCommit: original.OriginalCommit, RunID: 1, Attempt: 1, JobID: 2}
			candidate := BaselineCandidate{Envelope: envelope, PayloadSHA256: grant.PayloadSHA256}
			var source SourceAuthority = fixtureSourceAuthority{receipt: receipt}
			writer := fixtureWriterAuthority{grant: grant}
			switch scenario {
			case "unapproved":
				writer.denied = true
			case "transport":
				candidate.PayloadSHA256 = strings.Repeat("c", 64)
			case "output":
				writer.grant.OutputIdentity = strings.Repeat("c", 64)
			case "source-seal":
				candidate.Envelope.Binding.GitTree = requested.GitTree
				candidate.Envelope.ContentID = envelopeID(candidate.Envelope)
			case "writer-receipt":
				writer.grant.RunID = 0
			case "missing-authority":
				source = nil
			case "poisoned-baseline":
				require.NoError(t, os.WriteFile(filepath.Join(baseline, "source.rs"), []byte("forged"), 0600))
			}
			handoff, err := PrepareHandoff(context.Background(), candidate, source, writer, DefaultLimits())
			if scenario == "ready" {
				require.NoError(t, err)
				assert.True(t, handoff.Ready())
				assert.Equal(t, requested.CheckoutCommit, handoff.Requested().Binding.SourceCommit)
			} else {
				assert.Error(t, err)
				assert.False(t, handoff.Ready())
			}
		})
	}
}
