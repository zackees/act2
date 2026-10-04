//go:build linux

package artifactcache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	boltErrors "go.etcd.io/bbolt/errors"
)

func TestToolGenerationReaderProtectsOnlyItsOwnGeneration(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	reader, err := AcquireToolGenerationLease(context.Background(), store, first.ID, 100)
	require.NoError(t, err)
	defer reader.Close()
	blocked, err := openTransferLease(filepath.Join(first.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
	require.ErrorIs(t, err, boltErrors.ErrTimeout)
	require.Nil(t, blocked)
	secondReader, err := AcquireToolGenerationLease(context.Background(), store, first.ID, 100)
	require.NoError(t, err)
	require.NoError(t, secondReader.Close())
	spec.Installs[0].Path = "Node/2/x64"
	second := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, second.Partial, second.Error)
	require.NotEqual(t, first.ID, second.ID, "publishing another generation must work while the first is read")
	require.NoError(t, reader.Close())
	writer, err := openTransferLease(filepath.Join(first.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
}

func TestToolGenerationLeaseRefusesMissingCoordination(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	lock := filepath.Join(generation.Destination, toolGenerationReaderLock)
	require.NoError(t, os.Rename(lock, lock+".unavailable"))
	reader, err := AcquireToolGenerationLease(context.Background(), store, generation.ID, 100)
	require.Error(t, err)
	require.Nil(t, reader)
	_, err = os.Stat(lock)
	require.True(t, os.IsNotExist(err), "admission must never recreate a coordination file")
	retry := PublishToolGeneration(context.Background(), store, spec, 100)
	require.True(t, retry.Partial)
	require.False(t, retry.Published)
}

func TestToolGenerationLeaseHelperProcess(t *testing.T) {
	if os.Getenv("BOSN_TOOL_GENERATION_LEASE_HELPER") != "1" {
		t.Skip("owned helper subprocess")
	}
	reader, err := AcquireToolGenerationLease(context.Background(), os.Getenv("BOSN_TOOL_GENERATION_STORE"), os.Getenv("BOSN_TOOL_GENERATION_ID"), 100)
	require.NoError(t, err)
	defer reader.Close()
	fmt.Println("ready")
	var signal [1]byte
	_, _ = os.Stdin.Read(signal[:])
}

func TestToolGenerationReaderIsReleasedAfterHolderProcessDeath(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	binary, err := os.Executable()
	require.NoError(t, err)
	// #nosec G204 -- Re-exec this exact test binary with a fixed helper test; no shell or external command input.
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestToolGenerationLeaseHelperProcess$")
	cmd.Env = append(os.Environ(), "BOSN_TOOL_GENERATION_LEASE_HELPER=1", "BOSN_TOOL_GENERATION_STORE="+store, "BOSN_TOOL_GENERATION_ID="+generation.ID)
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
		t.Fatal("reader holder did not become ready")
	}
	lock := filepath.Join(generation.Destination, toolGenerationReaderLock)
	blocked, err := openTransferLease(lock, false, 100*time.Millisecond)
	require.ErrorIs(t, err, boltErrors.ErrTimeout)
	require.Nil(t, blocked)
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	writer, err := openTransferLease(lock, false, 100*time.Millisecond)
	require.NoError(t, err, "OS must release the exact killed holder's reader lease")
	require.NoError(t, writer.Close())
}

func TestToolGenerationAdmissionNeverRecreatesMissingCatalogCoordination(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	path := filepath.Join(store, toolStoreLock)
	original, err := openTransferLease(path, false, 100*time.Millisecond)
	require.NoError(t, err)
	defer original.Close()
	require.NoError(t, os.Rename(path, path+".held"))
	reader, err := AcquireToolGenerationLease(context.Background(), store, generation.ID, 100)
	if reader != nil {
		defer reader.Close()
	}
	require.Error(t, err, "admission must not replace a catalog mutex still held on another inode")
	require.Nil(t, reader)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "missing coordination must remain missing")
	retry := PublishToolGeneration(context.Background(), store, spec, 100)
	require.True(t, retry.Partial, "publishers must also refuse replacement catalog mutexes")
	require.False(t, retry.Published)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

func TestToolGenerationAdmissionDoesNotInitializeAnEmptyStore(t *testing.T) {
	store := t.TempDir()
	reader, err := AcquireToolGenerationLease(context.Background(), store, strings.Repeat("0", 64), 100)
	require.Error(t, err)
	require.Nil(t, reader)
	entries, err := os.ReadDir(store)
	require.NoError(t, err)
	require.Empty(t, entries)
}
