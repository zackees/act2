package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheToolUsageCommand(ctx context.Context, input *Input) *cobra.Command {
	var maxEntries int
	command := &cobra.Command{
		Use: "tool-usage", Short: "Audit unique inode allocation and apparent bytes in one coordinated tool store", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := artifactcache.AuditToolStoreUsage(ctx, input.cacheServerPath, maxEntries)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("tool store usage incomplete: %s", report.Error)
			}
			return nil
		},
	}
	command.Flags().IntVar(&maxEntries, "max-entries", 0, "Required inventory entry bound, 1..1000000; incomplete totals are unknown")
	return command
}
