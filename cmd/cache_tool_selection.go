package cmd

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheToolUpdateCommand(ctx context.Context, input *Input) *cobra.Command {
	return newCacheToolMutationCommand(ctx, input, toolGenerationUpdate)
}

func newCacheToolCurrentCommand(ctx context.Context, input *Input) *cobra.Command {
	var maxBytes int64
	var installs bool
	command := &cobra.Command{
		Use: "tool-current", Short: "Validate the selected warm generation (this report does not hold a reader lease)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if installs {
				state, err := artifactcache.CurrentToolGenerationState(ctx, input.cacheServerPath, maxBytes)
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
			}
			selection, err := artifactcache.CurrentToolGeneration(ctx, input.cacheServerPath, maxBytes)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(selection)
		},
	}
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive logical payload-byte bound for generation validation")
	command.Flags().BoolVar(&installs, "installs", false, "Include the exact verified current install set for successor planning")
	return command
}
