package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/nektos/act/pkg/common"
	"github.com/spf13/cobra"
)

// watchCohort waits after each bounded maintenance pass. A partial report is
// retried only in watch mode; single-pass callers retain a nonzero exit code.
func watchCohort(ctx context.Context, cmd *cobra.Command, root string, maxBytes int64, policy artifactcache.Policy, interval time.Duration) error {
	ctx, cancel := common.EarlyCancelContext(ctx)
	defer cancel()
	encoder := json.NewEncoder(cmd.OutOrStdout())
	for {
		if ctx.Err() != nil {
			return nil
		}
		report := artifactcache.MaintainCohort(ctx, root, maxBytes, policy)
		if err := encoder.Encode(report); err != nil {
			return err
		}
		if interval == 0 {
			if report.Partial {
				return fmt.Errorf("cohort retention is incomplete")
			}
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
