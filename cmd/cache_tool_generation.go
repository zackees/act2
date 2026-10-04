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
	var manifestPath string
	var maxBytes int64
	var apply bool
	command := &cobra.Command{Use: "tool-generation", Short: "Assemble a closed tool generation from immutable install objects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply {
				return fmt.Errorf("tool generation publication requires --apply")
			}
			spec, err := readToolGenerationSpec(manifestPath)
			if err != nil {
				return err
			}
			report := artifactcache.PublishToolGeneration(ctx, input.cacheServerPath, spec, maxBytes)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("tool generation incomplete: %s", report.Error)
			}
			return nil
		}}
	command.Flags().StringVar(&manifestPath, "manifest", "", "Schema-1 JSON listing install paths and closed object IDs (maximum 64 KiB)")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Positive logical payload-byte bound for the complete generation")
	command.Flags().BoolVar(&apply, "apply", false, "Validate objects and publish a generation sharing only closed file data")
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
