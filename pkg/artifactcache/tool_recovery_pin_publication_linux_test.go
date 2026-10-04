//go:build linux

package artifactcache

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolRecoveryPinPublicationIsImmutableAndRetryable(t *testing.T) {
	ctx := context.Background()
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, root, spec, 100)
	require.False(t, first.Partial, first.Error)
	now := time.Now().UTC()
	pin := ToolRecoveryPin{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
	report := PublishToolRecoveryPin(ctx, root, pin, 100)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	report = PublishToolRecoveryPin(ctx, root, pin, 100)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Reused)
	pin.ExpiresAt = pin.ExpiresAt.Add(time.Minute)
	report = PublishToolRecoveryPin(ctx, root, pin, 100)
	require.True(t, report.Partial)
	require.Contains(t, report.Error, "differs")
}

func TestToolRecoveryPinPublicationReconcilesAfterParentSyncFailure(t *testing.T) {
	ctx := context.Background()
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, root, spec, 100)
	require.False(t, first.Partial, first.Error)
	now := time.Now().UTC()
	pin := ToolRecoveryPin{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
	report := publishToolRecoveryPinWithSync(ctx, root, pin, 100, func(directory string) error {
		if directory == filepath.Join(root, toolRecoveryPinDirectory) {
			return errors.New("injected parent sync failure")
		}
		return syncToolDirectory(directory)
	})
	require.True(t, report.Published)
	require.True(t, report.Partial)
	report = PublishToolRecoveryPin(ctx, root, pin, 100)
	require.False(t, report.Partial, report.Error)
	require.True(t, report.Published)
	require.True(t, report.Reused)
}

func TestToolRecoveryPinValidationReleasesCatalogUnderOriginalReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, root, spec, 100)
	require.False(t, first.Partial, first.Error)
	now := time.Now().UTC()
	pin := ToolRecoveryPin{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan ToolRecoveryPinReport, 1)
	go func() {
		result <- publishToolRecoveryPinWithValidation(ctx, root, pin, 100, syncToolDirectory,
			func(ctx context.Context, generation string, data []byte, manifest toolManifest, maxBytes int64) error {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return verifyToolGeneration(ctx, generation, data, manifest, maxBytes)
			})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("validation did not start")
	}
	catalog, err := prepareExistingToolSnapshotStore(root)
	require.NoError(t, err, "validation must not serialize unrelated catalog operations")
	require.NoError(t, catalog.Close())
	writer, err := openTransferLease(filepath.Join(first.Generation.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
	if writer != nil {
		_ = writer.Close()
	}
	require.Error(t, err, "the original generation reader must protect payload validation")
	close(release)
	select {
	case report := <-result:
		require.False(t, report.Partial, report.Error)
		require.True(t, report.Published)
	case <-ctx.Done():
		t.Fatal("publication did not finish")
	}
}
