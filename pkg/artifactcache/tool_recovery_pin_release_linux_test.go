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

func TestToolRecoveryPinReleaseChecksIntentAndReconcilesSync(t *testing.T) {
	ctx := context.Background()
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, root, spec, 100)
	require.False(t, first.Partial, first.Error)
	now := time.Now().UTC()
	pin := ToolRecoveryPin{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
	published := PublishToolRecoveryPin(ctx, root, pin, 100)
	require.False(t, published.Partial, published.Error)
	changed := pin
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Minute)
	refused := ReleaseToolRecoveryPin(ctx, root, changed)
	require.True(t, refused.Partial)
	require.False(t, refused.Removed)
	require.FileExists(t, filepath.Join(root, toolRecoveryPinDirectory, pin.Owner+".json"))
	failed := releaseToolRecoveryPinWithSync(ctx, root, pin, func(string) error { return errors.New("injected release sync failure") })
	require.True(t, failed.Removed)
	require.True(t, failed.Partial)
	require.False(t, failed.Absent)
	retried := ReleaseToolRecoveryPin(ctx, root, pin)
	require.False(t, retried.Partial, retried.Error)
	require.True(t, retried.Absent)
	require.False(t, retried.Removed)
	require.True(t, ReleaseToolRecoveryPin(ctx, root, pin).Absent)
}

func TestToolRecoveryPinReleaseCancellationPreservesReference(t *testing.T) {
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(context.Background(), root, spec, 100)
	require.False(t, first.Partial, first.Error)
	now := time.Now().UTC()
	pin := ToolRecoveryPin{1, strings.Repeat("b", 64), first.Generation.ID, now, now.Add(time.Hour)}
	require.False(t, PublishToolRecoveryPin(context.Background(), root, pin, 100).Partial)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := ReleaseToolRecoveryPin(ctx, root, pin)
	require.True(t, report.Partial)
	require.False(t, report.Absent)
	require.False(t, report.Removed)
	require.FileExists(t, filepath.Join(root, toolRecoveryPinDirectory, pin.Owner+".json"))
}
