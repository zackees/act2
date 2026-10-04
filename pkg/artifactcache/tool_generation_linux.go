//go:build linux

package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const toolGenerationDirectory = ".tool-generations-v1"

type toolGenerationManifest struct {
	SchemaVersion int                     `json:"schema_version"`
	Installs      []ToolGenerationInstall `json:"installs"`
	Tree          toolManifest            `json:"tree"`
}

func publishToolGeneration(ctx context.Context, root string, spec ToolGenerationSpec, maxBytes int64) (report ToolSnapshotReport) {
	report = ToolSnapshotReport{SchemaVersion: 1, Source: root, Completion: "generation-v1"}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	installs, err := validateToolGenerationSpec(root, spec, maxBytes)
	if err != nil {
		report.fail(err)
		return report
	}
	lease, err := prepareToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer lease.Close()
	manifest, total, err := planToolGeneration(ctx, root, installs, maxBytes)
	if err != nil {
		report.fail(err)
		return report
	}
	data, err := json.Marshal(manifest)
	if err != nil || len(data) > toolManifestLimit {
		report.fail(fmt.Errorf("tool generation manifest exceeds bound"))
		return report
	}
	digest := sha256.Sum256(data)
	report.ID, report.Bytes = hex.EncodeToString(digest[:]), &total
	count := len(manifest.Tree.Entries)
	report.Entries = &count
	generations := filepath.Join(root, toolGenerationDirectory)
	if err := prepareToolGenerationDirectory(generations); err != nil {
		report.fail(err)
		return report
	}
	report.Destination = filepath.Join(generations, report.ID)
	if _, err := os.Lstat(report.Destination); err == nil {
		if err := verifyToolGeneration(ctx, report.Destination, data, manifest.Tree, maxBytes); err != nil {
			report.fail(err)
			return report
		}
		reader, err := openTransferLease(filepath.Join(report.Destination, toolGenerationReaderLock), true, 100*time.Millisecond)
		if err != nil {
			report.fail(err)
			return report
		}
		if err := reader.Close(); err != nil {
			report.fail(err)
			return report
		}
		report.Published, report.Reused = true, true
		if err := syncToolDirectory(generations); err != nil {
			report.fail(err)
		}
		return report
	} else if !os.IsNotExist(err) {
		report.fail(err)
		return report
	}
	stage, err := os.MkdirTemp(generations, ".tool-generation-stage-")
	if err != nil {
		report.fail(err)
		return report
	}
	report.PendingStage = stage
	defer func() {
		if report.PendingStage != "" {
			if err := os.RemoveAll(stage); err != nil {
				report.fail(fmt.Errorf("tool generation stage cleanup failed: %w", err))
			} else {
				report.PendingStage = ""
			}
		}
	}()
	if err := fillToolGeneration(ctx, root, stage, manifest, data, maxBytes); err != nil {
		report.fail(err)
		return report
	}
	if err := ctx.Err(); err != nil {
		report.fail(err)
		return report
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, report.Destination, unix.RENAME_NOREPLACE); err != nil {
		report.fail(err)
		return report
	}
	report.Published, report.PendingStage = true, ""
	if err := syncToolDirectory(generations); err != nil {
		report.fail(err)
	}
	return report
}

func validateToolGenerationSpec(root string, spec ToolGenerationSpec, maxBytes int64) ([]ToolGenerationInstall, error) {
	if maxBytes <= 0 || spec.SchemaVersion != 1 || len(spec.Installs) == 0 || len(spec.Installs) > 256 || len(root) > 4096 || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("tool generation requires a canonical store, schema 1, 1..256 installs and a positive byte bound")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, fmt.Errorf("tool generation store has missing or aliased ancestry")
	}
	// #nosec G703 -- Root is explicitly caller-selected and checked for canonical nonsymlink ancestry above.
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("tool generation store is not a directory")
	}
	installs := append([]ToolGenerationInstall(nil), spec.Installs...)
	sort.Slice(installs, func(i, j int) bool { return installs[i].Path < installs[j].Path })
	for index, install := range installs {
		if err := validateToolInstall(install); err != nil {
			return nil, err
		}
		for _, previous := range installs[:index] {
			if install.Path == previous.Path || strings.HasPrefix(install.Path, previous.Path+"/") {
				return nil, fmt.Errorf("tool generation install paths overlap")
			}
		}
	}
	return installs, nil
}

func validateToolInstall(install ToolGenerationInstall) error {
	if install.Path == "." || filepath.IsAbs(install.Path) || filepath.Clean(install.Path) != install.Path || len(install.Path) > 4096 || !utf8.ValidString(install.Path) || strings.HasPrefix(install.Path, "../") || install.Path == ".." {
		return fmt.Errorf("tool generation install path must be canonical and confined")
	}
	parts := strings.Split(install.Path, "/")
	if len(parts) > 48 || install.Path == "" {
		return fmt.Errorf("tool generation install path is empty or too deep")
	}
	for _, part := range parts {
		if strings.HasSuffix(part, ".complete") {
			return fmt.Errorf("tool generation path conflicts with completion markers")
		}
	}
	decoded, err := hex.DecodeString(install.ObjectID)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != install.ObjectID {
		return fmt.Errorf("tool generation object ID is not a canonical SHA-256")
	}
	return nil
}

func prepareToolGenerationDirectory(path string) error {
	if err := os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("tool generation directory is invalid")
	}
	return syncToolDirectory(filepath.Dir(path))
}
