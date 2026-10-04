package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheToolExecCommand(ctx context.Context, input *Input) *cobra.Command {
	var id string
	var maxBytes int64
	var apply bool
	command := &cobra.Command{
		Use: "tool-exec [flags] -- COMMAND [ARGS...]", Short: "Replace this engine-init process while holding a verified tool-generation reader",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !apply {
				return fmt.Errorf("tool generation exec requires --apply")
			}
			return artifactcache.ExecWithToolGeneration(ctx, input.cacheServerPath, id, maxBytes, args)
		},
	}
	command.Flags().StringVar(&id, "generation", "", "Canonical published generation ID (SHA-256)")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive logical payload-byte bound for generation validation")
	command.Flags().BoolVar(&apply, "apply", false, "Replace this dedicated init process, retaining the exact reader descriptor")
	return command
}
