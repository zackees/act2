package artifactcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const importReceiptFile = "import-receipt-v1.json"
const importReceiptLimit = 64 * 1024

// ImportPublicationReceipt records the verified import at namespace creation.
// It is not a current inventory, a retention outcome, or proof that the parent
// directory sync succeeded. A caller must reconcile publication and current
// archive state before enrollment. The source is never authorized for deletion.
type ImportPublicationReceipt struct {
	RetainedSourceArchiveBytes *int64          `json:"retained_source_archive_bytes"`
	SchemaVersion              int             `json:"schema_version"`
	Source                     string          `json:"source"`
	Destination                string          `json:"destination"`
	SourceFingerprint          string          `json:"source_fingerprint"`
	MaxBytes                   int64           `json:"max_bytes"`
	ImportedCount              uint64          `json:"imported_count"`
	ImportedBytes              int64           `json:"imported_bytes"`
	Receipts                   []ImportReceipt `json:"receipts"`
	ReceiptsOmitted            uint64          `json:"receipts_omitted"`
}

func writeImportPublicationReceipt(stage, fingerprint string, maxBytes int64, report *ImportReport) error {
	source, err := filepath.Abs(report.Source)
	if err != nil {
		return err
	}
	destination, err := filepath.Abs(report.Destination)
	if err != nil {
		return err
	}
	receipt := ImportPublicationReceipt{SchemaVersion: 1, Source: source, Destination: destination,
		RetainedSourceArchiveBytes: report.RetainedSourceArchiveBytes, SourceFingerprint: fingerprint, MaxBytes: maxBytes, ImportedCount: report.ImportedCount,
		ImportedBytes: report.ImportedBytes, Receipts: report.Receipts, ReceiptsOmitted: report.ReceiptsOmitted}
	if err := receipt.validate(destination); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > importReceiptLimit {
		return fmt.Errorf("import publication receipt exceeds bound")
	}
	// #nosec G703 -- generated private import stage, fixed receipt filename.
	file, err := os.OpenFile(filepath.Join(stage, importReceiptFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// ReadImportPublicationReceipt is bounded and read-only. Missing or invalid
// receipts remain errors; an existing namespace alone proves no warm import.
func ReadImportPublicationReceipt(dir string) (ImportPublicationReceipt, error) {
	var receipt ImportPublicationReceipt
	namespace, err := filepath.Abs(dir)
	if err != nil {
		return receipt, err
	}
	info, err := os.Lstat(namespace)
	if err != nil || !info.IsDir() {
		return receipt, fmt.Errorf("import namespace is not a readable directory")
	}
	marker, err := readCohortMarker(filepath.Join(namespace, cohortMarker))
	if err != nil || string(marker) != cohortIdentity {
		return receipt, fmt.Errorf("import namespace has no supported cohort marker")
	}
	path := filepath.Join(namespace, importReceiptFile)
	info, err = os.Lstat(path)
	if err != nil {
		return receipt, err
	}
	if !info.Mode().IsRegular() || info.Size() > importReceiptLimit {
		return receipt, fmt.Errorf("import receipt is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return receipt, fmt.Errorf("import receipt changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, importReceiptLimit+1))
	if err != nil {
		return receipt, err
	}
	if len(data) > importReceiptLimit {
		return receipt, fmt.Errorf("import receipt exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return receipt, fmt.Errorf("import receipt has trailing data")
	}
	return receipt, receipt.validate(namespace)
}

func (r ImportPublicationReceipt) validate(destination string) error {
	if r.SchemaVersion != 1 || r.Destination != destination || !filepath.IsAbs(r.Source) ||
		filepath.Clean(r.Source) != r.Source || r.Source == r.Destination || !receiptDigest(r.SourceFingerprint) ||
		r.RetainedSourceArchiveBytes == nil || *r.RetainedSourceArchiveBytes < r.ImportedBytes ||
		(*r.RetainedSourceArchiveBytes > 0 && r.ImportedCount == 0) ||
		r.MaxBytes <= 0 || r.ImportedBytes < 0 || r.ImportedBytes > r.MaxBytes || len(r.Receipts) > 12 ||
		r.ReceiptsOmitted > r.ImportedCount || uint64(len(r.Receipts)) != r.ImportedCount-r.ReceiptsOmitted {
		return fmt.Errorf("invalid import publication receipt identity or totals")
	}
	return r.validateArchives()
}

func (r ImportPublicationReceipt) validateArchives() error {
	sources, destinations := make(map[uint64]bool), make(map[uint64]bool)
	var total int64
	for _, row := range r.Receipts {
		if row.SourceID == 0 || row.DestinationID == 0 || sources[row.SourceID] || destinations[row.DestinationID] ||
			row.Bytes < 0 || row.Bytes > r.ImportedBytes-total || !receiptDigest(row.SHA256) {
			return fmt.Errorf("invalid import archive receipt")
		}
		sources[row.SourceID], destinations[row.DestinationID] = true, true
		total += row.Bytes
	}
	if r.ReceiptsOmitted == 0 && total != r.ImportedBytes {
		return fmt.Errorf("import receipt byte total mismatch")
	}
	return nil
}

func receiptDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
