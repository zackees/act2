package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/spf13/cobra"
)

func newCacheToolRecoveryCommand(ctx context.Context, input *Input) *cobra.Command {
	command := &cobra.Command{Use: "tool-recovery", Short: "Reserve or release a lower using durable recovery intent"}
	command.AddCommand(newCacheToolRecoveryMutation(ctx, input, false), newCacheToolRecoveryMutation(ctx, input, true))
	return command
}

func newCacheToolRecoveryMutation(ctx context.Context, input *Input, release bool) *cobra.Command {
	var record string
	var maxBytes int64
	var apply bool
	use := "reserve"
	if release {
		use = "release"
	}
	command := &cobra.Command{Use: use, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !apply {
			return fmt.Errorf("tool recovery mutation requires --apply")
		}
		pin, err := readToolRecoveryIntent(record)
		if err != nil {
			return err
		}
		if release {
			report := artifactcache.ReleaseToolRecoveryPin(ctx, input.cacheServerPath, pin)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Partial {
				return fmt.Errorf("tool recovery release incomplete: %s", report.Error)
			}
			return nil
		}
		report := artifactcache.PublishToolRecoveryPin(ctx, input.cacheServerPath, pin, maxBytes)
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
			return err
		}
		if report.Partial {
			return fmt.Errorf("tool recovery reservation incomplete: %s", report.Error)
		}
		return nil
	}}
	command.Flags().StringVar(&record, "record", "", "Required schema-1 frozen recovery intent JSON, maximum 1024 bytes")
	command.Flags().BoolVar(&apply, "apply", false, "Apply the explicit recovery mutation")
	if !release {
		command.Flags().Int64Var(&maxBytes, "max-bytes", 0, "Required positive complete-generation payload validation ceiling")
	}
	return command
}

func readToolRecoveryIntent(path string) (artifactcache.ToolRecoveryPin, error) {
	var pin artifactcache.ToolRecoveryPin
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
		return pin, fmt.Errorf("recovery intent must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return pin, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return pin, fmt.Errorf("recovery intent identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return pin, err
	}
	if len(data) > 1024 {
		return pin, fmt.Errorf("recovery intent exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pin); err != nil {
		return pin, err
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return pin, fmt.Errorf("recovery intent has trailing data")
	}
	return pin, nil
}
