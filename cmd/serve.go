package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nektos/act/pkg/serve"
	"github.com/spf13/cobra"
)

func noHook(*cobra.Command, []string) {}

func newServeCommand(ctx context.Context, version string) *cobra.Command {
	cfg := serve.Config{Version: version}
	root := &cobra.Command{
		Use:   "serve",
		Short: "Admit, isolate, run and clean up act runs inside a shared engine",
		Long: "Serve listens on a Unix socket and runs act for admitted runs, each in its own cgroup, " +
			"network, work tree and port pair; closing a run removes every object labelled with it.",
		Args:             cobra.NoArgs,
		PersistentPreRun: noHook, PersistentPostRun: noHook,
		RunE: func(*cobra.Command, []string) error {
			binary, err := os.Executable()
			if err != nil {
				return err
			}
			cfg.ActBinary = binary
			if err := cfg.Validate(); err != nil {
				return err
			}
			isolator, err := serve.NewIsolator(cfg)
			if err != nil {
				return err
			}
			server, err := serve.NewServer(cfg, isolator)
			if err != nil {
				return err
			}
			return server.Serve(ctx)
		},
	}
	root.PersistentFlags().StringVar(&cfg.Socket, "socket", serve.DefaultSocket, "Unix socket the server listens on and clients connect to")
	flags := root.Flags()
	flags.StringVar(&cfg.RunLabel, "run-label", "dev.act2.run", "Label whose value is the run ID on every object a run owns")
	flags.StringVar(&cfg.ScopePrefix, "scope-prefix", "act2-run-", "Prefix of each run's cgroup and network name")
	flags.StringVar(&cfg.WorkRoot, "work-root", "/var/lib/act2/runs", "Directory holding each run's work tree (owned by the server)")
	flags.IntVar(&cfg.PortBase, "port-base", 40000, "First port; slot n serves artifacts on base+2n and the cache on base+2n+1")
	flags.IntVar(&cfg.MaxRuns, "max-runs", 64, "Runs admitted at once")
	flags.StringVar(&cfg.DockerUpstream, "docker-upstream", "", "The engine's dockerd socket; with --docker-proxy-dir each run reaches it only through its own proxy")
	flags.StringVar(&cfg.ProxyDir, "docker-proxy-dir", "", "Directory of each run's Docker proxy socket, <key>.sock")
	flags.StringArrayVar(&cfg.SharedVolumes, "shared-volume", []string{"act-toolcache"}, "A named volume runs share (never keyed to a run or labelled)")
	flags.StringVar(&cfg.ToolCacheStore, "toolcache-store", "", "Shared tool-cache store to seed the engine's tool cache from and save completed installs to")
	flags.StringVar(&cfg.ToolCacheVolume, "toolcache-volume", "act-toolcache", "act's tool-cache volume")
	flags.StringVar(&cfg.ToolCacheMount, "toolcache-mount", "/var/lib/docker/volumes/act-toolcache/_data", "The tool-cache volume's directory in the engine")
	flags.StringVar(&cfg.Budget.Root, "cache-budget-root", "", "Shared cache root whose 16-hex namespaces get the cache budget (unset: none)")
	flags.StringVar(&cfg.Budget.Lock, "cache-budget-lock", ".act2-maintenance.lock", "Lock file, in the cache root, excluding other maintenance of it")
	flags.Int64Var(&cfg.Budget.MaxBytes, "cache-budget-max-bytes", 8<<30, "Completed archive bytes kept per namespace")
	flags.DurationVar(&cfg.Budget.UnusedAge, "cache-budget-unused-age", 7*24*time.Hour, "Archives unused this long expire")
	flags.DurationVar(&cfg.Budget.MaxAge, "cache-budget-max-age", 30*24*time.Hour, "Archives older than this expire")
	flags.DurationVar(&cfg.Budget.Interval, "cache-budget-interval", time.Hour, "Time between budget passes")
	flags.IntVar(&cfg.Budget.MaxNamespaces, "cache-budget-max-namespaces", 64, "Namespaces visited per pass; more is reported partial")
	client := func() *serve.Client { return serve.NewClient(cfg.Socket) }
	root.AddCommand(
		newServeStatusCommand(ctx, client),
		newServeAdmitCommand(ctx, client),
		newServeExecCommand(ctx, client),
		newServeRunCommand(ctx, "cancel", "Kill a run's processes", func(c *serve.Client, id string) error { return c.Cancel(ctx, id) }, client),
		newServeCloseCommand(ctx, client),
		newServePrepareCommand(ctx, client),
	)
	return root
}

func writeJSON(out io.Writer, value any) error {
	return json.NewEncoder(out).Encode(value)
}

func newServeStatusCommand(ctx context.Context, client func() *serve.Client) *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Print the server's health as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			health, err := client().Health(ctx)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), health)
		}}
}

func newServeAdmitCommand(ctx context.Context, client func() *serve.Client) *cobra.Command {
	return &cobra.Command{Use: "admit", Short: "Admit a run (AdmitRequest JSON on stdin) and print its scope", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var req serve.AdmitRequest
			decoder := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 1<<16))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				return fmt.Errorf("admit request: %w", err)
			}
			scope, err := client().Admit(ctx, req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), scope)
		}}
}

func newServeRunCommand(_ context.Context, use, short string, call func(*serve.Client, string) error, client func() *serve.Client) *cobra.Command {
	var runID string
	command := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return call(client(), runID) }}
	command.Flags().StringVar(&runID, "run", "", "Run ID")
	_ = command.MarkFlagRequired("run")
	return command
}

// execRequest builds the request from the command line: secret values come
// from this client's own environment (--env-from), never from argv.
func execRequest(args, envFrom, env []string, deadline int64) (serve.ExecRequest, error) {
	req := serve.ExecRequest{Args: args, Env: map[string]string{}, DeadlineSecs: deadline}
	for _, pair := range env {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return req, fmt.Errorf("--env %q is not NAME=VALUE", pair)
		}
		req.Env[key] = value
	}
	for _, key := range envFrom {
		value, ok := os.LookupEnv(key)
		if !ok {
			return req, fmt.Errorf("--env-from %s: not set in this environment", key)
		}
		req.Env[key] = value
	}
	return req, nil
}

func newServeExecCommand(ctx context.Context, client func() *serve.Client) *cobra.Command {
	var runID string
	var envFrom, env []string
	var deadline int64
	command := &cobra.Command{Use: "exec --run ID [flags] -- ACT_ARGS...", Short: "Run act in an admitted run's scope and exit with its code",
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := execRequest(args, envFrom, env, deadline)
			if err != nil {
				return err
			}
			end, err := client().Exec(ctx, runID, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if end.Error != "" {
				return fmt.Errorf("act serve exec: %s", end.Error)
			}
			if end.TimedOut {
				fmt.Fprintln(cmd.ErrOrStderr(), "act serve: the run's deadline passed; act was killed")
			}
			if end.Code != 0 {
				exitFunc(end.Code)
			}
			return nil
		}}
	command.Flags().StringVar(&runID, "run", "", "Run ID")
	_ = command.MarkFlagRequired("run")
	command.Flags().StringArrayVar(&envFrom, "env-from", nil, "Pass this variable from the client's environment to act")
	command.Flags().StringArrayVar(&env, "env", nil, "Pass NAME=VALUE to act")
	command.Flags().Int64Var(&deadline, "deadline", 0, "Kill act after this many seconds (0: none)")
	return command
}

func newServeCloseCommand(ctx context.Context, client func() *serve.Client) *cobra.Command {
	var runID string
	command := &cobra.Command{Use: "close", Short: "Remove a run, prove nothing of it is left, and print the tool-cache save as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := client().Close(ctx, runID)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}}
	command.Flags().StringVar(&runID, "run", "", "Run ID")
	_ = command.MarkFlagRequired("run")
	return command
}

func newServePrepareCommand(ctx context.Context, client func() *serve.Client) *cobra.Command {
	return &cobra.Command{Use: "prepare", Short: "Load the runner image and prepare the tool cache (PrepareRequest JSON on stdin)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var req serve.PrepareRequest
			decoder := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 4<<20))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				return fmt.Errorf("prepare request: %w", err)
			}
			out, err := client().Prepare(ctx, req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}}
}
