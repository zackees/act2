package artifactcache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestImportReceiptRejectsMissingMalformedAndWrongIdentity(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	report := ImportCompleted(context.Background(), source.dir, t.TempDir(), "repo", 80)
	require.False(t, report.Partial, report.Error)
	receipt, err := ReadImportPublicationReceipt(report.Destination)
	require.NoError(t, err)
	require.Equal(t, source.dir, receipt.Source)
	require.EqualValues(t, 80, receipt.MaxBytes)
	require.Equal(t, report.Receipts, receipt.Receipts)
	path := filepath.Join(report.Destination, importReceiptFile)
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	for name, change := range map[string]func(*ImportPublicationReceipt){
		"schema":      func(r *ImportPublicationReceipt) { r.SchemaVersion = 2 },
		"destination": func(r *ImportPublicationReceipt) { r.Destination = source.dir },
		"source":      func(r *ImportPublicationReceipt) { r.Source = "relative" },
		"budget":      func(r *ImportPublicationReceipt) { r.MaxBytes = 79 },
		"retained":    func(r *ImportPublicationReceipt) { r.RetainedSourceArchiveBytes = nil },
		"fingerprint": func(r *ImportPublicationReceipt) { r.SourceFingerprint = "unknown" },
		"count":       func(r *ImportPublicationReceipt) { r.ReceiptsOmitted = ^uint64(0) },
		"bytes":       func(r *ImportPublicationReceipt) { r.Receipts[0].Bytes = 79 },
		"digest":      func(r *ImportPublicationReceipt) { r.Receipts[0].SHA256 = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			var changed ImportPublicationReceipt
			require.NoError(t, json.Unmarshal(original, &changed))
			change(&changed)
			data, marshalErr := json.Marshal(changed)
			require.NoError(t, marshalErr)
			require.NoError(t, os.WriteFile(path, data, 0o600)) // #nosec G703 -- disposable test receipt.
			_, readErr := ReadImportPublicationReceipt(report.Destination)
			require.Error(t, readErr)
		})
	}
	for _, data := range [][]byte{
		append(append([]byte(nil), original...), []byte("{}")...),
		bytes.Repeat([]byte(" "), importReceiptLimit+1),
		[]byte(`{"unknown":true}`),
	} {
		require.NoError(t, os.WriteFile(path, data, 0o600)) // #nosec G703 -- disposable test receipt.
		_, err = ReadImportPublicationReceipt(report.Destination)
		require.Error(t, err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	_, err = ReadImportPublicationReceipt(missing)
	require.Error(t, err)
	_, err = os.Stat(missing)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The helper deliberately withholds its import report. Its parent terminates
// it after publication, modeling death before a successful acknowledgement.
func TestImportReceiptKilledProcessWorker(t *testing.T) {
	source := os.Getenv("ACT2_TEST_IMPORT_RECEIPT_SOURCE")
	if source == "" {
		t.Skip("subprocess helper")
	}
	report := ImportCompleted(context.Background(), source, os.Getenv("ACT2_TEST_IMPORT_RECEIPT_ROOT"), "repo", 80)
	require.False(t, report.Partial, report.Error)
	// #nosec G703 -- parent-selected readiness file inside its disposable test directory.
	require.NoError(t, os.WriteFile(os.Getenv("ACT2_TEST_IMPORT_RECEIPT_READY"), []byte("published"), 0o600))
	time.Sleep(time.Hour)
}

func TestImportReceiptSurvivesLostAcknowledgementAndProcessDeath(t *testing.T) {
	source := seedLegacyImportStore(t, 1)
	before, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	root := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestImportReceiptKilledProcessWorker$")
	child.Env = append(os.Environ(), "ACT2_TEST_IMPORT_RECEIPT_SOURCE="+source.dir,
		"ACT2_TEST_IMPORT_RECEIPT_ROOT="+root, "ACT2_TEST_IMPORT_RECEIPT_READY="+ready)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, child.Process.Kill())
	require.Error(t, child.Wait())
	require.Empty(t, output.String(), "no successful report was acknowledged")
	destination := filepath.Join(root, "repo")
	receipt, err := ReadImportPublicationReceipt(destination)
	require.NoError(t, err)
	require.EqualValues(t, 1, receipt.ImportedCount)
	require.EqualValues(t, 80, receipt.ImportedBytes)
	policy := DefaultPolicy()
	policy.CohortRoot = root
	server, err := StartHandlerWithPolicy(destination, "", "127.0.0.1", 0, nil, policy)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	assertArchiveHit(t, server, "warm-0")
	after, err := os.ReadFile(filepath.Join(source.dir, "bolt.db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
}
