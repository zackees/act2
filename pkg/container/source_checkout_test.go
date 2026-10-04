package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/sourcecheckout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
