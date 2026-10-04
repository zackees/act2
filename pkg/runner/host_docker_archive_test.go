package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archiveBoundaryPlan(t *testing.T, limits hostArchiveLimits) (*hostRestorePlan, string) {
	t.Helper()
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	plan := &hostRestorePlan{root: root, scratch: ".restore-test", archiveLimits: limits, originalTime: time.Now()}
	require.NoError(t, root.Mkdir(plan.scratch, 0700))
	require.NoError(t, root.Mkdir(plan.scratch+"/incoming", 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "original"), []byte("original"), 0600))
	t.Cleanup(func() { _ = plan.cleanup() })
	return plan, directory
}
func TestHostDockerArchiveBudgetBoundaryDoesNotCommitPrefix(t *testing.T) {
	plan, directory := archiveBoundaryPlan(t, hostArchiveLimits{bytes: 512})
	archive := restoreTestArchive(t, []*tar.Header{{Name: "prefix", Typeflag: tar.TypeReg, Mode: 0600}}, []string{""})
	defer archive.Close()
	err := plan.stage(context.Background(), archive)
	if err == nil {
		require.NoError(t, plan.apply(context.Background()))
	}
	assert.Error(t, err, "budget EOF is not archive completion")
	data, readErr := os.ReadFile(filepath.Join(directory, "original"))
	require.NoError(t, readErr)
	assert.Equal(t, "original", string(data))
}
func TestHostDockerArchiveTruncatedBoundaryDoesNotCommitPrefix(t *testing.T) {
	plan, directory := archiveBoundaryPlan(t, hostArchiveLimits{})
	archive := restoreTestArchive(t, []*tar.Header{{Name: "prefix", Typeflag: tar.TypeReg, Mode: 0600}}, []string{""})
	defer archive.Close()
	err := plan.stage(context.Background(), io.LimitReader(archive, 512))
	if err == nil {
		require.NoError(t, plan.apply(context.Background()))
	}
	assert.Error(t, err, "underlying EOF before end markers is incomplete")
	data, readErr := os.ReadFile(filepath.Join(directory, "original"))
	require.NoError(t, readErr)
	assert.Equal(t, "original", string(data))
}
func TestHostDockerArchiveMetadataBoundBeforeMutation(t *testing.T) {
	plan, directory := archiveBoundaryPlan(t, hostArchiveLimits{metadataBytes: 1024})
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "payload", Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": strings.Repeat("m", 4096)}}))
	require.NoError(t, writer.Close())
	err := plan.stage(context.Background(), bytes.NewReader(data.Bytes()))
	if err == nil {
		require.NoError(t, plan.apply(context.Background()))
	}
	assert.Error(t, err)
	contents, readErr := os.ReadFile(filepath.Join(directory, "original"))
	require.NoError(t, readErr)
	assert.Equal(t, "original", string(contents))
}

func TestHostDockerArchiveExactBudgetAndHeaderBoundary(t *testing.T) {
	archive := restoreTestArchive(t, []*tar.Header{{Name: "payload", Typeflag: tar.TypeReg, Mode: 0600}}, []string{""})
	defer archive.Close()
	plan, _ := archiveBoundaryPlan(t, hostArchiveLimits{bytes: 1536, headers: 1})
	require.NoError(t, plan.stage(context.Background(), archive))
}
func TestHostDockerArchiveHeaderBudgetRejectsOmittedTail(t *testing.T) {
	archive := restoreTestArchive(t, []*tar.Header{
		{Name: "first", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "second", Typeflag: tar.TypeReg, Mode: 0600},
	}, []string{"", ""})
	defer archive.Close()
	plan, directory := archiveBoundaryPlan(t, hostArchiveLimits{headers: 1})
	require.ErrorContains(t, plan.stage(context.Background(), archive), "header limit")
	data, err := os.ReadFile(filepath.Join(directory, "original"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
}
func TestHostDockerArchiveMetadataAggregateBound(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, name := range []string{"first", "second"} {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": strings.Repeat("m", 600)}}))
	}
	require.NoError(t, writer.Close())
	plan, directory := archiveBoundaryPlan(t, hostArchiveLimits{metadataBytes: 1024, metadataTotal: 1000})
	require.ErrorContains(t, plan.stage(context.Background(), bytes.NewReader(data.Bytes())), "metadata total")
	contents, err := os.ReadFile(filepath.Join(directory, "original"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(contents))
}
