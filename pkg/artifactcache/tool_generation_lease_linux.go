//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

const toolGenerationReaderLock = ".readers-v1.bolt"

func initializeToolGenerationReaderLock(stage string) error {
	// Only a private, unpublished stage may create this file. Admissions and
	// retirement use existing read-only descriptors and cannot recreate it.
	db, err := bbolt.Open(filepath.Join(stage, toolGenerationReaderLock), 0600, &bbolt.Options{Timeout: 100 * time.Millisecond})
	if err != nil {
		return err
	}
	return db.Close()
}

func acquireToolGenerationLease(ctx context.Context, root, id string, maxBytes int64) (ToolGenerationLease, error) {
	spec := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "lease", ObjectID: id}}}
	if _, err := validateToolGenerationSpec(root, spec, maxBytes); err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	decoded, _ := hex.DecodeString(id) // validated by the typed generation specification above
	id = hex.EncodeToString(decoded)
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		return nil, err
	}
	defer catalog.Close()
	generation := filepath.Join(root, toolGenerationDirectory, id)
	manifest, data, err := readToolGenerationManifest(generation, id)
	if err != nil {
		return nil, err
	}
	if _, err := validateToolGenerationSpec(root, ToolGenerationSpec{SchemaVersion: manifest.SchemaVersion, Installs: manifest.Installs}, maxBytes); err != nil {
		return nil, err
	}
	if err := verifyToolGeneration(ctx, generation, data, manifest.Tree, maxBytes); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openTransferLease(filepath.Join(generation, toolGenerationReaderLock), true, 100*time.Millisecond)
}

func readToolGenerationManifest(generation, id string) (toolGenerationManifest, []byte, error) {
	var manifest toolGenerationManifest
	// #nosec G703 -- Generation path is built from a validated canonical store and SHA-256 ID.
	parent, err := os.Lstat(filepath.Dir(generation))
	if err != nil || !parent.IsDir() {
		return manifest, nil, fmt.Errorf("tool generation catalog is invalid")
	}
	// #nosec G703 -- Generation path is built from a validated canonical store and SHA-256 ID.
	info, err := os.Lstat(generation)
	if err != nil || !info.IsDir() {
		return manifest, nil, fmt.Errorf("tool generation is missing or not closed")
	}
	path := filepath.Join(generation, "manifest.json")
	// #nosec G703 -- Fixed manifest leaf under the verified nonsymlink generation directory.
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > toolManifestLimit {
		return manifest, nil, fmt.Errorf("tool generation manifest is invalid or oversized")
	}
	// #nosec G703 -- Fixed bounded regular manifest under the verified generation directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != id {
		return manifest, nil, fmt.Errorf("tool generation manifest ID differs")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, nil, err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, data) || manifest.SchemaVersion != 1 || manifest.Tree.SchemaVersion != 1 || manifest.Tree.Completion != "generation-v1" || len(manifest.Tree.Entries) == 0 || len(manifest.Tree.Entries) > 100000 {
		return manifest, nil, fmt.Errorf("tool generation manifest is not canonical schema 1")
	}
	return manifest, data, nil
}
