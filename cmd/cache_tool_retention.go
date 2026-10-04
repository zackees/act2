package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/spf13/cobra"
)

func newCacheToolRetentionCommand(ctx context.Context, input *Input) *cobra.Command {
	var policy artifactcache.ToolRetentionPolicy
	var expireBefore string
	var apply bool
	command := &cobra.Command{Use: "tool-retain", Short: "Expire verified shared tool stages, generations and unreferenced objects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply {
				return fmt.Errorf("tool retention requires --apply; use tool-usage for inventory")
			}
			cutoff, err := time.Parse(time.RFC3339Nano, expireBefore)
			if err != nil {
				return fmt.Errorf("expire-before requires an explicit RFC3339 timestamp: %w", err)
			}
			policy.ExpireBefore = cutoff
			report := artifactcache.RetainToolStore(ctx, input.cacheServerPath, policy)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("tool retention incomplete: %s", report.Error)
			}
			if report.ProtectedOverflow {
				return fmt.Errorf("tool retention protected overflow: retained state exceeds allocated-byte cap")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "Apply bounded age/pressure retention; required for mutation")
	command.Flags().StringVar(&expireBefore, "expire-before", "", "Required RFC3339 publication/stage creation cutoff")
	command.Flags().Int64Var(&policy.MaxAllocatedBytes, "max-allocated-bytes", 0, "Required positive store inode-allocation ceiling")
	command.Flags().Int64Var(&policy.MaxPayloadBytes, "max-payload-bytes", 0, "Required positive payload validation ceiling")
	command.Flags().IntVar(&policy.MaxEntries, "max-entries", 0, "Required store inventory entry bound, 1..1000000")
	command.Flags().IntVar(&policy.MaxCandidates, "max-candidates", 0, "Required per-namespace candidate bound, 1..10000")
	return command
}
