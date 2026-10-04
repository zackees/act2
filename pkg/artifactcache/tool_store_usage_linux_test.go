//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestToolStoreUsageCountsHardlinkedGenerationsOnce(t *testing.T) {
	source, store := completedToolFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tool"), bytes.Repeat([]byte("x"), 1<<20), 0600))
	object := PublishToolSnapshot(context.Background(), source, store, 10<<20)
	require.False(t, object.Partial, object.Error)
	spec := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: object.ID}}}
	first := PublishToolGeneration(context.Background(), store, spec, 10<<20)
	require.False(t, first.Partial, first.Error)
	spec.Installs = append(spec.Installs, ToolGenerationInstall{Path: "Node/2/x64", ObjectID: object.ID})
	second := PublishToolGeneration(context.Background(), store, spec, 10<<20)
	require.False(t, second.Partial, second.Error)
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, bytes.Repeat([]byte("z"), 5<<20), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(store, "outside-link")))
	report := AuditToolStoreUsage(context.Background(), store, 10000)
	require.False(t, report.Partial, report.Error)
	require.NotNil(t, report.UniqueFileBytes)
	require.NotNil(t, report.ReferencedFileBytes)
	require.Equal(t, int64(3<<20), *report.ReferencedFileBytes-*report.UniqueFileBytes, "four payload paths share one inode")
	require.Greater(t, report.PathEntries, report.UniqueInodes)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, metric := range []struct {
		args     []string
		expected *int64
	}{
		{[]string{"--summarize", "--block-size=1", store}, report.AllocatedBytes},
		{[]string{"--summarize", "--apparent-size", "--block-size=1", store}, report.ApparentBytes},
	} {
		// #nosec G204 -- Fixed du executable and literal options over this test's private store; no shell is inserted.
		output, err := exec.CommandContext(ctx, "du", metric.args...).Output()
		require.NoError(t, err)
		value, err := strconv.ParseInt(strings.Fields(string(output))[0], 10, 64)
		require.NoError(t, err)
		require.NotNil(t, metric.expected)
		require.Equal(t, value, *metric.expected, "independent du must agree, including metadata and non-followed symlinks")
	}
}

func TestToolStoreUsageDistinguishesSparseApparentAndAllocatedBytes(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	path := filepath.Join(store, "sparse")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	require.NoError(t, os.Truncate(path, 16<<20))
	report := AuditToolStoreUsage(context.Background(), store, 10000)
	require.False(t, report.Partial, report.Error)
	require.GreaterOrEqual(t, *report.ApparentBytes, int64(16<<20))
	require.Less(t, *report.AllocatedBytes, *report.ApparentBytes)
}

func TestToolStoreUsageRefusesUnknownTotalsOnIncompleteInventory(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	for _, scenario := range []string{"entry-bound", "invalid-bound", "cancelled", "missing-catalog"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			bound := 10000
			switch scenario {
			case "entry-bound":
				bound = 1
			case "invalid-bound":
				bound = 0
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing-catalog":
				path := filepath.Join(store, toolStoreLock)
				require.NoError(t, os.Rename(path, path+".unavailable"))
			}
			report := AuditToolStoreUsage(ctx, store, bound)
			require.True(t, report.Partial)
			require.Nil(t, report.ApparentBytes)
			require.Nil(t, report.AllocatedBytes)
			require.Nil(t, report.UniqueFileBytes)
			require.Nil(t, report.ReferencedFileBytes)
		})
	}
}
