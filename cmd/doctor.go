package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nektos/act/pkg/doctor"
	"github.com/nektos/act/pkg/serve"
	"github.com/spf13/cobra"
)

func newDoctorCommand(ctx context.Context) *cobra.Command {
	var asJSON bool
	var socket string
	var diskGiB, memoryGiB float64
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Check, read-only, whether this host can run the shared engine and its runs",
		Long: "Doctor starts nothing. Each check has a stable ID, says whether it is required, and gives a status " +
			"(pass, warn, fail, skip), a summary and a remediation. It exits nonzero only when a required check fails.",
		Args:             cobra.NoArgs,
		PersistentPreRun: noHook, PersistentPostRun: noHook,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := doctor.Run(ctx, doctor.HostProbes(socket), doctor.Options{
				MinDiskBytes:   uint64(diskGiB * (1 << 30)),
				MinMemoryBytes: int64(memoryGiB * (1 << 30)),
			})
			out := cmd.OutOrStdout()
			if asJSON {
				if err := json.NewEncoder(out).Encode(report); err != nil {
					return err
				}
			} else {
				for _, c := range report.Checks {
					need := "optional"
					if c.Required {
						need = "required"
					}
					fmt.Fprintf(out, "%-4s %-19s %-8s %s\n", c.Status, c.ID, need, c.Summary)
					if c.Remediation != "" && c.Status != doctor.Pass {
						fmt.Fprintf(out, "     fix: %s\n", c.Remediation)
					}
				}
			}
			if !report.OK {
				exitFunc(1)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "Print one JSON report")
	command.Flags().StringVar(&socket, "socket", serve.DefaultSocket, "act serve socket to report on, when it exists")
	command.Flags().Float64Var(&diskGiB, "min-disk-gib", 20, "Free disk under Docker's data root below which disk.headroom warns")
	command.Flags().Float64Var(&memoryGiB, "min-memory-gib", 4, "Memory below which memory.headroom warns")
	return command
}
