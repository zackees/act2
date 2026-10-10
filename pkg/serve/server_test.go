//go:build !windows

package serve

import (
	"bytes"
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIsolator struct {
	mu        sync.Mutex
	opened    []string
	closed    []string
	reaped    int
	failClose bool
}

func (f *fakeIsolator) Open(_ context.Context, s Scope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, s.RunID)
	return nil
}
func (f *fakeIsolator) Prepare(*exec.Cmd, Scope) (func(), error) { return func() {}, nil }
func (f *fakeIsolator) Kill(Scope) error                         { return nil }
func (f *fakeIsolator) Close(_ context.Context, s Scope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClose {
		return assert.AnError
	}
	f.closed = append(f.closed, s.RunID)
	return nil
}
func (f *fakeIsolator) Reap(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reaped++
	return nil
}

const (
	runA = "0a1b2c3d-4e5f-4a6b-8c7d-000000000001"
	runB = "1a1b2c3d-4e5f-4a6b-8c7d-000000000002"
	runC = "2a1b2c3d-4e5f-4a6b-8c7d-000000000003"
)

var limits = Limits{MemoryBytes: 1 << 30, NanoCPUs: 1_000_000_000, Pids: 256}

func start(t *testing.T, maxRuns int) (*Client, *fakeIsolator) {
	t.Helper()
	dir := t.TempDir()
	iso := &fakeIsolator{}
	server, err := NewServer(Config{
		Socket: filepath.Join(dir, "s.sock"), RunLabel: "test.run", ScopePrefix: "t-",
		WorkRoot: filepath.Join(dir, "runs"), PortBase: 40000, MaxRuns: maxRuns, ActBinary: "/bin/sh", Version: "test",
	}, iso)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	client := NewClient(filepath.Join(dir, "s.sock"))
	require.Eventually(t, func() bool { _, err := client.Health(ctx); return err == nil }, 5*time.Second, 10*time.Millisecond)
	return client, iso
}

func status(err error) int {
	if s, ok := err.(*StatusError); ok {
		return s.Status
	}
	return 0
}

func TestAdmissionAllocatesSlotsAndRefusesWhenFull(t *testing.T) {
	ctx := context.Background()
	client, iso := start(t, 2)
	a, err := client.Admit(ctx, AdmitRequest{RunID: runA, Limits: limits})
	require.NoError(t, err)
	assert.Equal(t, Scope{RunID: runA, Key: a.Key, Slot: 0, Cgroup: "/t-" + a.Key, Network: "t-" + a.Key,
		Work: a.Work, ArtifactPort: 40000, CachePort: 40001, Limits: limits}, a)
	assert.Equal(t, strings.ReplaceAll(runA, "-", "")[:12], a.Key)
	assert.DirExists(t, filepath.Join(a.Work, "home/.cache"))
	again, err := client.Admit(ctx, AdmitRequest{RunID: runA, Limits: limits})
	require.NoError(t, err)
	assert.Equal(t, a, again, "admission is idempotent")
	other := limits
	other.Pids = 999
	_, err = client.Admit(ctx, AdmitRequest{RunID: runA, Limits: other})
	assert.Equal(t, http.StatusConflict, status(err))
	b, err := client.Admit(ctx, AdmitRequest{RunID: runB, Limits: limits})
	require.NoError(t, err)
	assert.Equal(t, 1, b.Slot)
	_, err = client.Admit(ctx, AdmitRequest{RunID: runC, Limits: limits})
	assert.Equal(t, http.StatusServiceUnavailable, status(err))
	require.NoError(t, client.Close(ctx, runA))
	c, err := client.Admit(ctx, AdmitRequest{RunID: runC, Limits: limits})
	require.NoError(t, err)
	assert.Equal(t, 0, c.Slot, "a closed run's slot is reused")
	assert.Equal(t, []string{runA, runB, runC}, iso.opened)
	assert.Equal(t, []string{runA}, iso.closed)
	assert.Equal(t, 1, iso.reaped)
	_, err = client.Admit(ctx, AdmitRequest{RunID: "nope", Limits: limits})
	assert.Equal(t, http.StatusBadRequest, status(err))
	_, err = client.Admit(ctx, AdmitRequest{RunID: runA})
	assert.Equal(t, http.StatusBadRequest, status(err))
}

func TestExecStreamsOutputAndExitCode(t *testing.T) {
	ctx := context.Background()
	client, _ := start(t, 1)
	scope, err := client.Admit(ctx, AdmitRequest{RunID: runA, Limits: limits})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	end, err := client.Exec(ctx, runA, ExecRequest{
		Args: []string{"-c", `pwd; echo "$SECRET"; echo "$HOME"; echo oops >&2; exit 3`},
		Env:  map[string]string{"SECRET": "s3cret"},
	}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, Exit{Code: 3}, end)
	assert.Equal(t, filepath.Join(scope.Work, "src")+"\ns3cret\n"+filepath.Join(scope.Work, "home")+"\n", stdout.String())
	assert.Equal(t, "oops\n", stderr.String())
	_, err = client.Exec(ctx, runB, ExecRequest{Args: []string{"-c", "true"}}, &stdout, &stderr)
	assert.Equal(t, http.StatusNotFound, status(err))
}

func TestDeadlineKillsAct(t *testing.T) {
	ctx := context.Background()
	client, _ := start(t, 1)
	_, err := client.Admit(ctx, AdmitRequest{RunID: runA, Limits: limits})
	require.NoError(t, err)
	var out bytes.Buffer
	end, err := client.Exec(ctx, runA, ExecRequest{Args: []string{"-c", "exec sleep 30"}, DeadlineSecs: 1}, &out, &out)
	require.NoError(t, err)
	assert.True(t, end.TimedOut)
	assert.Equal(t, "killed", end.Signal)
	assert.Equal(t, 137, end.Code)
}

func TestCloseStopsARunningExecAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client, iso := start(t, 2)
	for _, run := range []string{runA, runB} {
		_, err := client.Admit(ctx, AdmitRequest{RunID: run, Limits: limits})
		require.NoError(t, err)
	}
	ends := make(chan Exit, 1)
	started := make(chan struct{})
	go func() {
		end, _ := client.Exec(ctx, runA, ExecRequest{Args: []string{"-c", "echo started; exec sleep 30"}}, signalWriter(started), &bytes.Buffer{})
		ends <- end
	}()
	<-started
	_, err := client.Exec(ctx, runA, ExecRequest{Args: []string{"-c", "true"}}, &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, http.StatusConflict, status(err), "one exec at a time")
	require.NoError(t, client.Close(ctx, runA))
	select {
	case end := <-ends:
		assert.Equal(t, "killed", end.Signal)
	case <-time.After(10 * time.Second):
		t.Fatal("close did not stop the exec")
	}
	require.NoError(t, client.Close(ctx, runA), "closing again is a no-op")
	health, err := client.Health(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{runB}, health.Runs, "the other run is untouched")
	assert.Equal(t, []string{runA}, iso.closed)
}

func TestAFailedCloseKeepsTheRunForARetry(t *testing.T) {
	ctx := context.Background()
	client, iso := start(t, 1)
	_, err := client.Admit(ctx, AdmitRequest{RunID: runA, Limits: limits})
	require.NoError(t, err)
	iso.failClose = true
	assert.Equal(t, http.StatusInternalServerError, status(client.Close(ctx, runA)))
	_, err = client.Admit(ctx, AdmitRequest{RunID: runB, Limits: limits})
	assert.Equal(t, http.StatusServiceUnavailable, status(err), "the slot stays held")
	iso.failClose = false
	require.NoError(t, client.Close(ctx, runA))
	_, err = client.Admit(ctx, AdmitRequest{RunID: runB, Limits: limits})
	require.NoError(t, err)
}

type signalWriter chan struct{}

func (s signalWriter) Write(p []byte) (int, error) {
	select {
	case <-s:
	default:
		close(s)
	}
	return len(p), nil
}
