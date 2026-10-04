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
	cache.AddCommand(audit, prune)
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
