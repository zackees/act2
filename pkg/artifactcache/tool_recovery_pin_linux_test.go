//go:build linux

package artifactcache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression contract: a durable, unexpired recovery reference must protect
// the immutable lower after every process reader has disappeared. This fixture
// writes the proposed canonical record directly; it is not a publication API.
func TestToolRetentionPreservesDurableRecoveryLowerWithoutLivingReader(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	now := time.Now().UTC()
	record := struct {
		SchemaVersion int       `json:"schema_version"`
		Owner         string    `json:"owner"`
		Generation    string    `json:"generation"`
		CreatedAt     time.Time `json:"created_at"`
		ExpiresAt     time.Time `json:"expires_at"`
	}{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
	pins := filepath.Join(store, ".tool-recovery-pins-v1")
	require.NoError(t, os.Mkdir(pins, 0700))
	data, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(pins, record.Owner+".json"), data, 0600))
	policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: now.Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
	report := RetainToolStore(ctx, store, policy)
	require.False(t, report.Partial, report.Error)
	_, err = os.Stat(first.Generation.Destination)
	require.NoError(t, err, "pending recovery lost its immutable lower despite an unexpired durable reference")
	require.Contains(t, report.ProtectedGenerations, first.Generation.ID)
	require.True(t, report.ProtectedOverflow)
}

func TestToolRecoveryPinsRejectAmbiguousStateAndPermitExpiry(t *testing.T) {
	for _, scenario := range []string{"expired", "malformed", "owner-mismatch", "future-created", "missing-lower", "symlink", "oversized", "direct-retirement"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, spec := toolGenerationFixture(t)
			first := InitializeToolGeneration(ctx, store, spec, 100)
			require.False(t, first.Partial, first.Error)
			next := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
			require.False(t, next.Partial, next.Error)
			now := time.Now().UTC()
			pin := ToolRecoveryPin{1, strings.Repeat("a", 64), first.Generation.ID, now, now.Add(time.Hour)}
			if scenario == "expired" {
				pin.CreatedAt = now.Add(-2 * time.Hour)
				pin.ExpiresAt = now.Add(-time.Hour)
			}
			if scenario == "owner-mismatch" {
				pin.Owner = strings.Repeat("b", 64)
			}
			if scenario == "future-created" {
				pin.CreatedAt = now.Add(time.Hour)
				pin.ExpiresAt = now.Add(2 * time.Hour)
			}
			if scenario == "missing-lower" {
				pin.Generation = strings.Repeat("f", 64)
			}
			data, err := json.Marshal(pin)
			require.NoError(t, err)
			if scenario == "malformed" {
				data = []byte("{")
			}
			if scenario == "oversized" {
				data = []byte(strings.Repeat("x", 1025))
			}
			directory := filepath.Join(store, toolRecoveryPinDirectory)
			require.NoError(t, os.Mkdir(directory, 0700))
			path := filepath.Join(directory, strings.Repeat("a", 64)+".json")
			if scenario == "symlink" {
				external := filepath.Join(t.TempDir(), "pin")
				require.NoError(t, os.WriteFile(external, data, 0600))
				require.NoError(t, os.Symlink(external, path))
			} else {
				require.NoError(t, os.WriteFile(path, data, 0600))
			}
			if scenario == "direct-retirement" {
				require.ErrorContains(t, RetireToolGeneration(ctx, store, first.Generation.ID, 100), "pending recovery")
			} else {
				policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: now.Add(time.Hour), MaxEntries: 10000, MaxCandidates: 100, MaxPayloadBytes: 100}
				report := RetainToolStore(ctx, store, policy)
				if scenario == "expired" {
					require.False(t, report.Partial, report.Error)
					require.Contains(t, report.RetiredGenerations, first.Generation.ID)
					_, err = os.Stat(first.Generation.Destination)
					require.True(t, os.IsNotExist(err))
					_, err = os.Stat(path)
					require.True(t, os.IsNotExist(err), "expired recovery metadata must not accumulate")
					return
				}
				require.True(t, report.Partial)
				require.Empty(t, report.RetiredGenerations)
				require.Empty(t, report.RetiredObjects)
			}
			_, err = os.Stat(first.Generation.Destination)
			require.NoError(t, err)
		})
	}
}

func TestToolRecoveryPinExpiryIsBoundedAndPreservesActiveReference(t *testing.T) {
	ctx := context.Background()
	root, spec := toolGenerationFixture(t)
	first := InitializeToolGeneration(ctx, root, spec, 100)
	require.False(t, first.Partial, first.Error)
	next := UpdateToolGeneration(ctx, root, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Node/2/x64", ObjectID: spec.Installs[0].ObjectID}}}, 100)
	require.False(t, next.Partial, next.Error)
	now := time.Now().UTC()
	active := ToolRecoveryPin{1, fmt.Sprintf("%064x", 42), first.Generation.ID, now, now.Add(time.Hour)}
	published := PublishToolRecoveryPin(ctx, root, active, 100)
	require.False(t, published.Partial, published.Error)
	for index := 1; index <= 41; index++ {
		pin := ToolRecoveryPin{1, fmt.Sprintf("%064x", index), first.Generation.ID, now.Add(-time.Hour), now.Add(-time.Minute)}
		data, err := json.Marshal(pin)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, toolRecoveryPinDirectory, pin.Owner+".json"), data, 0600))
	}
	policy := ToolRetentionPolicy{MaxAllocatedBytes: 1, ExpireBefore: now.Add(time.Hour), MaxEntries: 10000, MaxCandidates: 8, MaxPayloadBytes: 100}
	report := RetainToolStore(ctx, root, policy)
	require.False(t, report.Partial, report.Error)
	require.Equal(t, uint64(8), report.ExpiredPins)
	require.Equal(t, uint64(33), report.RemainingExpiredPins)
	require.Contains(t, report.ProtectedGenerations, first.Generation.ID)
	policy.MaxCandidates = 100
	report = RetainToolStore(ctx, root, policy)
	require.False(t, report.Partial, report.Error)
	require.Equal(t, uint64(33), report.ExpiredPins)
	require.Zero(t, report.RemainingExpiredPins)
	require.Len(t, report.ExpiredPinOwners, 32)
	require.Equal(t, uint64(1), report.ExpiredPinOwnersOmitted)
	entries, err := os.ReadDir(filepath.Join(root, toolRecoveryPinDirectory))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, active.Owner+".json", entries[0].Name())
	require.Contains(t, report.ProtectedGenerations, first.Generation.ID)
}
