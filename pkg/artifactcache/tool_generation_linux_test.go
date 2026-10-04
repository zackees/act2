//go:build linux

package artifactcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func toolGenerationFixture(t *testing.T) (string, ToolGenerationSpec) {
	t.Helper()
	source, store := completedToolFixture(t)
	object := PublishToolSnapshot(context.Background(), source, store, 100)
	require.False(t, object.Partial, object.Error)
	return store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}}}
}

func TestToolGenerationRejectsInvalidAndUnboundedRequests(t *testing.T) {
	for _, scenario := range []string{"schema", "empty", "absolute", "escape", "overlap", "marker", "object-id", "missing", "budget", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			store, spec := toolGenerationFixture(t)
			ctx := context.Background()
			limit := int64(100)
			switch scenario {
			case "schema":
				spec.SchemaVersion = 2
			case "empty":
				spec.Installs = nil
			case "absolute":
				spec.Installs[0].Path = "/tool"
			case "escape":
				spec.Installs[0].Path = "../tool"
			case "overlap":
				spec.Installs = append(spec.Installs, ToolGenerationInstall{Path: "Go", ObjectID: spec.Installs[0].ObjectID})
			case "marker":
				spec.Installs[0].Path = "Go/1/x64.complete"
			case "object-id":
				spec.Installs[0].ObjectID = "../source"
			case "missing":
				spec.Installs[0].ObjectID = strings.Repeat("0", 64)
			case "budget":
				limit = 3
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			report := PublishToolGeneration(ctx, store, spec, limit)
			require.True(t, report.Partial, scenario)
			require.False(t, report.Published)
			_, err := os.Stat(filepath.Join(store, toolGenerationDirectory))
			require.True(t, os.IsNotExist(err), "invalid generation must not create generation storage")
		})
	}
}

func TestToolGenerationRejectsUnexpectedExistingPayload(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	first := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, first.Partial, first.Error)
	foreign := filepath.Join(first.Destination, "tree", "foreign")
	require.NoError(t, os.WriteFile(foreign, []byte("keep"), 0600))
	retry := PublishToolGeneration(context.Background(), store, spec, 100)
	require.True(t, retry.Partial)
	require.False(t, retry.Published)
	require.Equal(t, first.ID, retry.ID)
	actual, err := os.ReadFile(foreign)
	require.NoError(t, err)
	require.Equal(t, "keep", string(actual))
}

func TestToolGenerationProvidesReaderLeaseBeforePublicationSuccess(t *testing.T) {
	store, spec := toolGenerationFixture(t)
	generation := PublishToolGeneration(context.Background(), store, spec, 100)
	require.False(t, generation.Partial, generation.Error)
	reader, err := openTransferLease(filepath.Join(generation.Destination, ".readers-v1.bolt"), true, 100*time.Millisecond)
	require.NoError(t, err, "a published generation must already provide reader coordination")
	require.NoError(t, reader.Close())
}
