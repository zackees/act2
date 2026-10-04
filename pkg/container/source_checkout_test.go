package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/sourcecheckout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rejectingSourceReceiver struct{ calls int }

func (receiver *rejectingSourceReceiver) MaterializedHandoff(_ context.Context, destination, _ string, _ bool) (SourceHandoffInput, error) {
	receiver.calls++
	// A transport may have materialized bytes before admission rejects it.
	if err := os.WriteFile(filepath.Join(destination, "stale.rs"), []byte("untrusted"), 0600); err != nil {
		return SourceHandoffInput{}, err
	}
	return SourceHandoffInput{}, fmt.Errorf("untrusted transport")
}

func TestActualCopyDirDiscardsRejectedInitialMaterialization(t *testing.T) {
	source, owned := t.TempDir(), t.TempDir()
	destination := filepath.Join(owned, "workspace")
	require.NoError(t, os.Mkdir(destination, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "source.rs"), []byte("requested"), 0600))
	foreign := filepath.Join(owned, "foreign")
	require.NoError(t, os.WriteFile(foreign, []byte("untouched"), 0600))
	receiver := &rejectingSourceReceiver{}
	environment := HostEnvironment{Path: destination, OwnedRoot: owned, Workdir: source, SourceReceiver: receiver}
	require.NoError(t, environment.CopyDir(destination, source+string(filepath.Separator)+".", false)(context.Background()))
	assert.Equal(t, 1, receiver.calls)
	_, err := os.Stat(filepath.Join(destination, "stale.rs"))
	assert.True(t, os.IsNotExist(err))
	body, err := os.ReadFile(filepath.Join(destination, "source.rs"))
	require.NoError(t, err)
	assert.Equal(t, "requested", string(body))
	body, err = os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(body))
	// Subsequent copies never admit another donor into the active workspace.
	require.NoError(t, environment.CopyDir(destination, source, false)(context.Background()))
	assert.Equal(t, 1, receiver.calls)
}

func TestActualCopyDirNeverAdmitsNestedOrUnownedReceiver(t *testing.T) {
	for _, scenario := range []string{"nested", "unowned", "git-ignore-profile"} {
		t.Run(scenario, func(t *testing.T) {
			source, owned := t.TempDir(), t.TempDir()
			destination := filepath.Join(owned, "workspace")
			require.NoError(t, os.Mkdir(destination, 0700))
			requested := source
			if scenario == "nested" {
				requested = filepath.Join(source, "nested")
				require.NoError(t, os.Mkdir(requested, 0700))
			}
			require.NoError(t, os.WriteFile(filepath.Join(requested, "source.rs"), []byte("cold"), 0600))
			receiver := &rejectingSourceReceiver{}
			environment := HostEnvironment{Path: destination, OwnedRoot: owned, Workdir: source, SourceReceiver: receiver}
			if scenario == "unowned" {
				environment.OwnedRoot = ""
			}
			require.NoError(t, environment.CopyDir(destination, requested, scenario == "git-ignore-profile")(context.Background()))
			assert.Zero(t, receiver.calls)
			assert.FileExists(t, filepath.Join(destination, "source.rs"))
			if scenario == "git-ignore-profile" {
				// The unsupported initial profile still consumes admission. Later
				// copies must never replace an already active cold workspace.
				require.NoError(t, environment.CopyDir(destination, requested, false)(context.Background()))
				assert.Zero(t, receiver.calls)
			}
		})
	}
}

func TestSourceHandoffFallbackUsesActualCopyDir(t *testing.T) {
	for _, scenario := range []string{"missing-authority", "unowned-destination", "nested-source"} {
		t.Run(scenario, func(t *testing.T) {
			source := t.TempDir()
			owned := t.TempDir()
			destination := filepath.Join(owned, "workspace")
			require.NoError(t, os.Mkdir(destination, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(source, "source.rs"), []byte("same"), 0600))
			target := filepath.Join(destination, "source.rs")
			require.NoError(t, os.WriteFile(target, []byte("same"), 0600))
			old := time.Unix(1000, 123456789)
			require.NoError(t, os.Chtimes(target, old, old))
			before, err := os.Stat(target)
			require.NoError(t, err)
			environment := HostEnvironment{Path: destination, OwnedRoot: owned, Workdir: source}
			requested := source
			if scenario == "unowned-destination" {
				environment.OwnedRoot = ""
			}
			if scenario == "nested-source" {
				requested = filepath.Join(source, "nested")
			}
			handoff, err := environment.PrepareSourceHandoff(context.Background(), destination, requested, sourcecheckout.BaselineCandidate{}, nil, nil, sourcecheckout.DefaultLimits())
			assert.Error(t, err)
			assert.False(t, handoff.Ready())
			// Ordinary checkout is still the production fallback. Identical bytes write
			// again here; no warm behavior is claimed from a rejected receipt.
			require.NoError(t, environment.CopyDir(destination, source+string(filepath.Separator)+".", false)(context.Background()))
			after, err := os.Stat(target)
			require.NoError(t, err)
			assert.NotEqual(t, before.ModTime(), after.ModTime())
			body, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "same", string(body))
		})
	}
}

func TestActualCopyDirCancelledAdmissionDoesNotCopyRequestedSource(t *testing.T) {
	source, owned := t.TempDir(), t.TempDir()
	destination := filepath.Join(owned, "workspace")
	require.NoError(t, os.Mkdir(destination, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "source.rs"), []byte("requested"), 0600))
	receiver := &rejectingSourceReceiver{}
	environment := HostEnvironment{Path: destination, OwnedRoot: owned, Workdir: source, SourceReceiver: receiver}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, environment.CopyDir(destination, source, false)(ctx), context.Canceled)
	_, err := os.Stat(filepath.Join(destination, "source.rs"))
	assert.True(t, os.IsNotExist(err))
}
