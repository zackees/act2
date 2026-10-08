package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nektos/act/pkg/artifactcache"
)

func newCacheToolGenerationCommand(ctx context.Context, input *Input) *cobra.Command {
	return newCacheToolMutationCommand(ctx, input, toolGenerationPublication)
}

type toolGenerationMutationKind uint8

const (
	toolGenerationPublication toolGenerationMutationKind = iota
	toolGenerationUpdate
)

func newCacheToolMutationCommand(ctx context.Context, input *Input, kind toolGenerationMutationKind) *cobra.Command {
	var manifestPath, expectedGeneration string
	var maxBytes int64
	var apply, initialize, replace bool
	use, description := "tool-generation", "Assemble a closed tool generation from immutable install objects"
	if kind == toolGenerationUpdate {
		use, description = "tool-update", "Merge closed install updates into the latest selected warm generation"
	}
	command := &cobra.Command{Use: use, Short: description, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply {
				return fmt.Errorf("tool generation mutation requires --apply")
			}
			if expectedGeneration != "" && !replace {
				return fmt.Errorf("expected generation requires --replace")
			}
			spec, err := readToolGenerationSpec(manifestPath)
			if err != nil {
				return err
			}
			var payload any
			var partial bool
			var message string
			if kind == toolGenerationUpdate {
				var report artifactcache.ToolGenerationUpdateReport
				if initialize {
					report = artifactcache.InitializeToolGeneration(ctx, input.cacheServerPath, spec, maxBytes)
				} else if replace {
					report = artifactcache.ReplaceToolGeneration(ctx, input.cacheServerPath, expectedGeneration, spec, maxBytes)
				} else {
					report = artifactcache.UpdateToolGeneration(ctx, input.cacheServerPath, spec, maxBytes)
				}
				payload, partial, message = report, report.Partial, report.Error
			} else {
				report := artifactcache.PublishToolGeneration(ctx, input.cacheServerPath, spec, maxBytes)
				payload, partial, message = report, report.Partial, report.Error
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(payload); err != nil {
				return err
			}
			if partial {
				return fmt.Errorf("tool generation mutation incomplete: %s", message)
			}
			return nil
		}}
	command.Flags().StringVar(&manifestPath, "manifest", "", "Schema-1 JSON listing closed install paths and object IDs (maximum 64 KiB)")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive logical payload-byte bound for the complete generation")
	command.Flags().BoolVar(&apply, "apply", false, "Validate objects and publish the requested immutable generation mutation")
	if kind == toolGenerationUpdate {
		command.Flags().BoolVar(&initialize, "initialize", false, "Explicitly bootstrap an unset selection; never an automatic missing-cache fallback")
		command.Flags().BoolVar(&replace, "replace", false, "Select exactly the supplied install set; caller coordinates a bounded complete successor")
		command.Flags().StringVar(&expectedGeneration, "expected-generation", "", "Selected generation ID that this exact successor replaces; stale selections are refused")
		command.MarkFlagsMutuallyExclusive("initialize", "replace")
		command.MarkFlagsRequiredTogether("replace", "expected-generation")
	}
	return command
}

func readToolGenerationSpec(path string) (artifactcache.ToolGenerationSpec, error) {
	var spec artifactcache.ToolGenerationSpec
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return spec, fmt.Errorf("tool generation specification must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return spec, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return spec, err
	}
	if len(data) > 64*1024 {
		return spec, fmt.Errorf("tool generation specification exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, err
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return spec, fmt.Errorf("tool generation specification has trailing data")
	}
	return spec, nil
}
