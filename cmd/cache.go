package cmd

import (
	"context"
	"encoding/json"
	"fmt"

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
	cohort := &cobra.Command{Use: "prune-cohort", Short: "Apply aggregate retention to an enrolled idle cohort", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cohortApply {
				return fmt.Errorf("aggregate retention requires --apply; use cache audit for individual namespaces")
			}
			report := artifactcache.MaintainCohort(ctx, input.cacheServerPath, cohortMax, input.cachePolicy)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("cohort retention is incomplete")
			}
			return nil
		}}
	cohort.Flags().Int64Var(&cohortMax, "max-bytes", 0, "Aggregate completed archive ceiling across the cohort; 0 disables it")
	cohort.Flags().BoolVar(&cohortApply, "apply", false, "Apply age, namespace and aggregate retention to this cohort")
	cache.AddCommand(audit, prune, cohort)
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
