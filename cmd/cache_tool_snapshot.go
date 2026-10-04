package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheToolSnapshotCommand(ctx context.Context, input *Input) *cobra.Command {
	var source string
	var maxBytes int64
	var apply, quiescent bool
	command := &cobra.Command{Use: "tool-publish", Short: "Publish one completed quiescent tool install as a closed object", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply || !quiescent {
				return fmt.Errorf("tool publication requires --apply and --source-quiescent")
			}
			report := artifactcache.PublishToolSnapshot(ctx, source, input.cacheServerPath, maxBytes)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("tool publication incomplete: %s", report.Error)
			}
			return nil
		}}
	command.Flags().StringVar(&source, "from", "", "Completed install directory; never modified or hard-linked into published data")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive bound for the complete object; no partial warm object is published")
	command.Flags().BoolVar(&apply, "apply", false, "Copy, validate and durably publish an immutable object")
	command.Flags().BoolVar(&quiescent, "source-quiescent", false, "Confirm no source writers remain; marker existence alone cannot establish this")
	return command
}
