package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
)

func TestCohortWatchRetriesIncompletePassesUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := createRootCommand(ctx, &Input{}, "test")
	output := &cancelAfterReports{cancel: cancel, count: 3}
	root.SetOut(output)
	dir := filepath.Join(t.TempDir(), "missing")
	root.SetArgs([]string{"cache", "prune-cohort", "--apply", "--max-bytes", "100", "--watch", "1ms", "--cache-server-path", dir})
	require.NoError(t, root.Execute())
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for i := 0; i < 3; i++ {
		var report artifactcache.CohortReport
		require.NoError(t, decoder.Decode(&report))
		require.True(t, report.Partial)
		require.Nil(t, report.BudgetMet)
	}
}

type cancelAfterReports struct {
	bytes.Buffer
	cancel context.CancelFunc
	count  int
}

func (w *cancelAfterReports) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.count--
	if w.count == 0 {
		w.cancel()
	}
	return n, err
}

func TestCohortWatchRejectsInvalidIntervalBeforeMaintenance(t *testing.T) {
	for _, interval := range []string{"-1s", "0s"} {
		root := createRootCommand(context.Background(), &Input{}, "test")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"cache", "prune-cohort", "--apply", "--watch", interval, "--cache-server-path", t.TempDir()})
		require.Error(t, root.Execute())
		require.Empty(t, out.String())
	}
}

func TestCohortWatchCanceledBeforeFirstPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := createRootCommand(ctx, &Input{}, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"cache", "prune-cohort", "--apply", "--watch", time.Minute.String(), "--cache-server-path", t.TempDir()})
	require.NoError(t, root.Execute())
	require.Empty(t, out.String())
}

func TestCohortWatchResumesAfterActiveServerReleasesLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	policy := artifactcache.DefaultPolicy()
	policy.CohortRoot = dir
	h, err := artifactcache.StartHandlerWithPolicy(filepath.Join(dir, "repo"), "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	defer h.Close()
	root := createRootCommand(ctx, &Input{}, "test")
	var passes []artifactcache.CohortReport
	output := &reportCallback{after: func(data []byte) {
		var report artifactcache.CohortReport
		require.NoError(t, json.Unmarshal(data, &report))
		passes = append(passes, report)
		if len(passes) == 1 {
			require.NoError(t, h.Close())
		} else {
			cancel()
		}
	}}
	root.SetOut(output)
	root.SetArgs([]string{"cache", "prune-cohort", "--apply", "--max-bytes", "100", "--watch", "1ms", "--cache-server-path", dir})
	require.NoError(t, root.Execute())
	require.Len(t, passes, 2)
	require.True(t, passes[0].Partial)
	require.False(t, passes[1].Partial, passes[1].Errors)
	require.True(t, *passes[1].BudgetMet)
	require.EqualValues(t, 0, *passes[1].RemainingCompletedBytes)
}

type reportCallback struct{ after func([]byte) }

func (w *reportCallback) Write(p []byte) (int, error) { w.after(p); return len(p), nil }
