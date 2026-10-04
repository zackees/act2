package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nektos/act/pkg/artifactcache"
	"github.com/stretchr/testify/require"
)

// Exercise command routing and actual cache HTTP transfers in the layout Bosn
// will use. This proves command interoperability, not daemon supervision.
func TestCacheCutoverCommandsPreserveFreshServerHitsAndRepositoryIsolation(t *testing.T) {
	volume := t.TempDir()
	cohort := filepath.Join(volume, "actcache", "cohort-v1")
	repositories := []string{"0123456789abcdef", "fedcba9876543210"}
	contents := [][]byte{bytes.Repeat([]byte("a"), 80), bytes.Repeat([]byte("b"), 80)}
	client := &http.Client{Timeout: 2 * time.Second}
	for index, repository := range repositories {
		source := filepath.Join(volume, "actcache", repository)
		server, err := artifactcache.StartHandler(source, "", "127.0.0.1", 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		uploadCutoverArchive(t, client, server.ExternalURL(), contents[index])
		require.NoError(t, server.Close())
		before, err := os.ReadFile(filepath.Join(source, "bolt.db"))
		require.NoError(t, err)
		output := runCutoverCommand(t, "cache", "import", "--apply", "--source-quiescent",
			"--from", source, "--namespace", repository, "--max-bytes", "80", "--cache-server-path", cohort)
		var report artifactcache.ImportReport
		require.NoError(t, json.Unmarshal(output, &report))
		require.False(t, report.Partial, report.Error)
		require.True(t, report.Published)
		require.EqualValues(t, 80, report.ImportedBytes)
		require.EqualValues(t, 80, *report.RetainedSourceArchiveBytes)
		var receipt artifactcache.ImportPublicationReceipt
		require.NoError(t, json.Unmarshal(runCutoverCommand(t, "cache", "import-receipt",
			"--cache-server-path", report.Destination), &receipt))
		require.Equal(t, source, receipt.Source)
		require.Equal(t, report.Destination, receipt.Destination)
		require.Equal(t, report.Receipts, receipt.Receipts)
		after, err := os.ReadFile(filepath.Join(source, "bolt.db"))
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	output := runCutoverCommand(t, "cache", "prune-cohort", "--apply", "--max-bytes", "80", "--cache-server-path", cohort)
	var retention artifactcache.CohortReport
	require.NoError(t, json.Unmarshal(output, &retention))
	require.False(t, retention.Partial, retention.Errors)
	require.NotNil(t, retention.BudgetMet)
	require.False(t, *retention.BudgetMet, "recently transferred archives must remain protected")
	require.EqualValues(t, 160, *retention.RemainingCompletedBytes)
	require.EqualValues(t, 160, *retention.ProtectedBytes)
	for cycle := 0; cycle < 3; cycle++ {
		for index, repository := range repositories {
			policy := artifactcache.DefaultPolicy()
			policy.CohortRoot = cohort
			server, err := artifactcache.StartHandlerWithPolicy(filepath.Join(cohort, repository), "", "127.0.0.1", 0, nil, policy)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			assertCutoverArchive(t, client, server.ExternalURL(), contents[index])
			require.NoError(t, server.Close())
		}
	}
}

func runCutoverCommand(t *testing.T, args ...string) []byte {
	t.Helper()
	root := createRootCommand(context.Background(), &Input{}, "test")
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs(args)
	require.NoError(t, root.Execute())
	return output.Bytes()
}

func cutoverRequest(t *testing.T, client *http.Client, method, url string, body []byte, contentRange string) []byte {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	if contentRange != "" {
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Content-Range", contentRange)
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(data))
	return data
}

func uploadCutoverArchive(t *testing.T, client *http.Client, externalURL string, content []byte) {
	t.Helper()
	base := externalURL + "/_apis/artifactcache"
	body, err := json.Marshal(artifactcache.Request{Key: "same-key", Version: "v", Size: int64(len(content))})
	require.NoError(t, err)
	var reservation struct {
		ID uint64 `json:"cacheId"`
	}
	require.NoError(t, json.Unmarshal(cutoverRequest(t, client, http.MethodPost, base+"/caches", body, ""), &reservation))
	require.NotZero(t, reservation.ID)
	target := fmt.Sprintf("%s/caches/%d", base, reservation.ID)
	cutoverRequest(t, client, http.MethodPatch, target, content, fmt.Sprintf("bytes 0-%d/*", len(content)-1))
	cutoverRequest(t, client, http.MethodPost, target, nil, "")
}

func assertCutoverArchive(t *testing.T, client *http.Client, externalURL string, expected []byte) {
	t.Helper()
	var hit struct {
		Location string `json:"archiveLocation"`
		Key      string `json:"cacheKey"`
	}
	lookup := cutoverRequest(t, client, http.MethodGet, externalURL+"/_apis/artifactcache/cache?keys=same-key&version=v", nil, "")
	require.NoError(t, json.Unmarshal(lookup, &hit))
	require.Equal(t, "same-key", hit.Key)
	require.Equal(t, expected, cutoverRequest(t, client, http.MethodGet, hit.Location, nil, ""))
}
