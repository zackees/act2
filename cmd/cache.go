package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheCommand(ctx context.Context, input *Input) *cobra.Command {
	cache := &cobra.Command{Use: "cache", Short: "Inspect or maintain a cache namespace",
		PersistentPreRun: func(*cobra.Command, []string) {}, PersistentPostRun: func(*cobra.Command, []string) {}}
	var cursor uint64
	audit := &cobra.Command{Use: "audit", Short: "Export a consistent bounded cache catalog", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeStoreAudit(cmd, artifactcache.AuditStore(ctx, input.cacheServerPath, cursor))
		}}
	audit.Flags().Uint64Var(&cursor, "cursor", 0, "Continue after this archive ID; page fingerprints must match")
	var apply bool
	prune := &cobra.Command{Use: "prune", Short: "Inspect a namespace, or apply retention with --apply", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := artifactcache.AuditStore(ctx, input.cacheServerPath, 0)
			if apply {
				report = artifactcache.MaintainStore(ctx, input.cacheServerPath, input.cachePolicy)
			}
			return writeStoreAudit(cmd, report)
		}}
	prune.Flags().BoolVar(&apply, "apply", false, "Apply the configured cache-server byte/age policy to this namespace")
	var cohortMax int64
	var cohortApply bool
	var cohortWatch time.Duration
	cohort := &cobra.Command{Use: "prune-cohort", Short: "Apply aggregate retention to an enrolled idle cohort", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cohortApply {
				return fmt.Errorf("aggregate retention requires --apply; use cache audit for individual namespaces")
			}
			if cmd.Flags().Changed("watch") && cohortWatch <= 0 {
				return fmt.Errorf("watch interval must be positive")
			}
			if cohortMax < 0 {
				return fmt.Errorf("aggregate maximum bytes must not be negative")
			}
			if err := input.cachePolicy.Validate(); err != nil {
				return err
			}
			return watchCohort(ctx, cmd, input.cacheServerPath, cohortMax, input.cachePolicy, cohortWatch)
		}}
	cohort.Flags().Int64Var(&cohortMax, "max-bytes", 0, "Aggregate completed archive ceiling across the cohort; 0 disables it")
	cohort.Flags().DurationVar(&cohortWatch, "watch", 0, "Retry aggregate maintenance periodically, including incomplete or busy passes; emits one JSON report per pass")
	cohort.Flags().BoolVar(&cohortApply, "apply", false, "Apply age, namespace and aggregate retention to this cohort")
	cache.AddCommand(audit, prune, cohort, newCacheImportCommand(ctx, input))
	return cache
}

func writeStoreAudit(cmd *cobra.Command, report artifactcache.StoreAudit) error {
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
		return err
	}
	if report.Partial {
		return fmt.Errorf("cache %s audit is %s", report.Namespace, report.Status)
	}
	return nil
}

func newCacheImportCommand(ctx context.Context, input *Input) *cobra.Command {
	var source, name string
	var maxBytes int64
	var apply, quiescent bool
	command := &cobra.Command{Use: "import", Short: "Copy completed archives from a quiescent legacy store into a new cohort namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply || !quiescent {
				return fmt.Errorf("cache import requires --apply and --source-quiescent; stop legacy servers before importing")
			}
			report := artifactcache.ImportCompleted(ctx, source, input.cacheServerPath, name, maxBytes)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("cache import incomplete: %s", report.Error)
			}
			return nil
		}}
	command.Flags().StringVar(&source, "from", "", "Existing quiescent legacy namespace; it is never modified")
	command.Flags().StringVar(&name, "namespace", "", "New direct-child namespace name in the destination cohort")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive bound for imported completed archives; recent entries are selected first")
	command.Flags().BoolVar(&apply, "apply", false, "Copy, validate and atomically publish the new namespace")
	command.Flags().BoolVar(&quiescent, "source-quiescent", false, "Confirm legacy servers are stopped; metadata locking can block their requests")
	return command
}
