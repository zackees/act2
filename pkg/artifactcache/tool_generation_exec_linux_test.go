//go:build linux

package artifactcache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	boltErrors "go.etcd.io/bbolt/errors"
	"golang.org/x/sys/unix"
)

func TestToolGenerationExecHelper(t *testing.T) {
	phase := os.Getenv("BOSN_TOOL_EXEC_TEST_PHASE")
	if phase == "" {
		t.Skip("owned exec helper")
	}
	if phase == "before" {
		binary, err := os.Executable()
		require.NoError(t, err)
		require.NoError(t, os.Setenv("BOSN_TOOL_EXEC_TEST_PHASE", "after"))
		err = ExecWithToolGeneration(context.Background(), os.Getenv("BOSN_TOOL_EXEC_TEST_STORE"), os.Getenv("BOSN_TOOL_EXEC_TEST_ID"), 100, []string{binary, "-test.run=^TestToolGenerationExecHelper$"})
		t.Fatalf("exec must replace the holder process: %v", err)
	}
	require.Equal(t, "after", phase)
	fd, err := strconv.Atoi(os.Getenv("BOSN_TOOL_GENERATION_LEASE_FD"))
	require.NoError(t, err)
	var stat unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &stat))
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(os.Getenv("BOSN_TOOL_EXEC_TEST_STORE"), toolGenerationDirectory, os.Getenv("BOSN_TOOL_EXEC_TEST_ID"), toolGenerationReaderLock), path)
	fmt.Println("ready")
	var signal [1]byte
	_, _ = os.Stdin.Read(signal[:])
}

func TestToolGenerationExecRetainsExactPublishedReaderInode(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	binary, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 -- Re-exec the exact test binary with a fixed helper test.
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestToolGenerationExecHelper$")
	cmd.Env = append(os.Environ(), "BOSN_TOOL_EXEC_TEST_PHASE=before", "BOSN_TOOL_EXEC_TEST_STORE="+store, "BOSN_TOOL_EXEC_TEST_ID="+generation.ID)
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer input.Close()
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		require.Equal(t, "ready\n", line)
	case <-ctx.Done():
		t.Fatal("exec holder did not become ready")
	}
	lock := filepath.Join(generation.Destination, toolGenerationReaderLock)
	writer, err := openTransferLease(lock, false, 100*time.Millisecond)
	require.ErrorIs(t, err, boltErrors.ErrTimeout)
	require.Nil(t, writer)
	require.NoError(t, input.Close())
	require.NoError(t, cmd.Wait())
	writer, err = openTransferLease(lock, false, 100*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
}

func TestToolGenerationExecRefusalsReleaseReader(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	invalidExecutable := filepath.Join(t.TempDir(), "not-an-executable-format")
	require.NoError(t, os.WriteFile(invalidExecutable, []byte("invalid executable\n"), 0600))
	require.NoError(t, os.Chmod(invalidExecutable, 0700))
	for _, command := range [][]string{nil, {"bad\x00argument"}, {invalidExecutable}} {
		err := ExecWithToolGeneration(context.Background(), store, generation.ID, 100, command)
		require.Error(t, err)
		writer, err := openTransferLease(filepath.Join(generation.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
		require.NoError(t, err, "a refused or failed exec must release its reader")
		require.NoError(t, writer.Close())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, ExecWithToolGeneration(ctx, store, generation.ID, 100, []string{invalidExecutable}))
	writer, err := openTransferLease(filepath.Join(generation.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
}
