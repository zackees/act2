//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	boltErrors "go.etcd.io/bbolt/errors"
)

func TestToolGenerationAdmissionValidatesUnderReaderWithoutCatalog(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	openReader := func(path string) (ToolGenerationLease, error) {
		return openTransferLease(path, true, 100*time.Millisecond)
	}
	called := false
	verify := func(ctx context.Context, path string, data []byte, tree toolManifest, bound int64) error {
		called = true
		catalog, err := prepareExistingToolSnapshotStore(store)
		if err != nil {
			return fmt.Errorf("payload validation still holds catalog exclusion: %w", err)
		}
		require.NoError(t, catalog.Close())
		writer, err := openTransferLease(filepath.Join(generation.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
		if writer != nil {
			_ = writer.Close()
		}
		require.ErrorIs(t, err, boltErrors.ErrTimeout, "reader must protect generation throughout payload validation")
		otherReader, err := AcquireToolGenerationLease(ctx, store, generation.ID, bound)
		require.NoError(t, err, "another admission must proceed during payload validation")
		require.NoError(t, otherReader.Close())
		otherSpec := ToolGenerationSpec{SchemaVersion: 1, Installs: append([]ToolGenerationInstall(nil), spec.Installs...)}
		otherSpec.Installs[0].Path = "Other/2/x64"
		published := PublishToolGeneration(ctx, store, otherSpec, bound)
		require.False(t, published.Partial, published.Error)
		return verifyToolGeneration(ctx, path, data, tree, bound)
	}
	reader, err := acquireToolGenerationLeaseWithValidation(context.Background(), store, generation.ID, 100, openReader, verify)
	if reader != nil {
		defer reader.Close()
	}
	require.True(t, called)
	require.NoError(t, err)
	require.NotNil(t, reader)
}

func TestToolGenerationAdmissionFailedValidationReleasesReader(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	opened := false
	openReader := func(path string) (ToolGenerationLease, error) {
		opened = true
		return openTransferLease(path, true, 100*time.Millisecond)
	}
	verify := func(context.Context, string, []byte, toolManifest, int64) error { return fmt.Errorf("corrupt payload") }
	reader, err := acquireToolGenerationLeaseWithValidation(context.Background(), store, generation.ID, 100, openReader, verify)
	require.ErrorContains(t, err, "corrupt payload")
	require.Nil(t, reader)
	require.True(t, opened, "reader must be acquired before payload validation")
	writer, err := openTransferLease(filepath.Join(generation.Destination, toolGenerationReaderLock), false, 100*time.Millisecond)
	require.NoError(t, err, "failed admission must release reader")
	require.NoError(t, writer.Close())
}
