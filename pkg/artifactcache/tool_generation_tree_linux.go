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
	"sort"
	"strings"
)

func planToolGeneration(ctx context.Context, root string, installs []ToolGenerationInstall, maxBytes int64) (toolGenerationManifest, int64, error) {
	manifest := toolGenerationManifest{SchemaVersion: 1, Installs: installs, Tree: toolManifest{SchemaVersion: 1, Completion: "generation-v1"}}
	entries := map[string]toolEntry{".": syntheticToolEntry(".", "directory")}
	var total int64
	for _, install := range installs {
		if err := ctx.Err(); err != nil {
			return manifest, total, err
		}
		object, err := loadToolGenerationObject(ctx, filepath.Join(root, install.ObjectID), install.ObjectID, maxBytes-total)
		if err != nil {
			return manifest, total, err
		}
		for parent := filepath.Dir(install.Path); parent != "."; parent = filepath.Dir(parent) {
			entries[parent] = syntheticToolEntry(parent, "directory")
		}
		if object.Completion == "sibling-v1" {
			entries[install.Path+".complete"] = syntheticToolEntry(install.Path+".complete", "file")
		}
		for _, entry := range object.Entries {
			entry.Path = filepath.Join(install.Path, entry.Path)
			if _, exists := entries[entry.Path]; exists {
				return manifest, total, fmt.Errorf("tool generation entry collision")
			}
			if entry.Bytes > maxBytes-total {
				return manifest, total, fmt.Errorf("tool generation exceeds byte bound")
			}
			total += entry.Bytes
			entries[entry.Path] = entry
		}
		if len(entries) > 100000 {
			return manifest, total, fmt.Errorf("tool generation entry bound exceeded")
		}
	}
	for _, entry := range entries {
		manifest.Tree.Entries = append(manifest.Tree.Entries, entry)
	}
	sort.Slice(manifest.Tree.Entries, func(i, j int) bool { return manifest.Tree.Entries[i].Path < manifest.Tree.Entries[j].Path })
	return manifest, total, nil
}

func syntheticToolEntry(path, kind string) toolEntry {
	mode := uint32(0755)
	if kind == "file" {
		mode = 0600
	}
	entry := toolEntry{Path: path, Kind: kind, Mode: mode, UID: os.Getuid(), GID: os.Getgid()}
	if kind == "file" {
		digest := sha256.Sum256(nil)
		entry.Digest = hex.EncodeToString(digest[:])
	}
	return entry
}

func loadToolGenerationObject(ctx context.Context, object, id string, maxBytes int64) (toolManifest, error) {
	var manifest toolManifest
	info, err := os.Lstat(object)
	if err != nil || !info.IsDir() {
		return manifest, fmt.Errorf("tool generation object is missing or not closed")
	}
	path := filepath.Join(object, "manifest.json")
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > toolManifestLimit {
		return manifest, fmt.Errorf("tool generation object manifest is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != id {
		return manifest, fmt.Errorf("tool generation object manifest ID differs")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.SchemaVersion != 1 || (manifest.Completion != "inner-v1" && manifest.Completion != "sibling-v1") {
		return manifest, fmt.Errorf("tool generation object schema is unsupported")
	}
	return manifest, verifyToolObject(ctx, object, data, manifest.Completion, maxBytes)
}

func fillToolGeneration(ctx context.Context, root, stage string, manifest toolGenerationManifest, data []byte, maxBytes int64) error {
	tree := filepath.Join(stage, "tree")
	if err := os.Mkdir(tree, 0755); err != nil {
		return err
	}
	for _, entry := range manifest.Tree.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Path == "." {
			continue
		}
		if err := linkToolGenerationEntry(root, tree, manifest.Installs, entry); err != nil {
			return err
		}
	}
	for index := len(manifest.Tree.Entries) - 1; index >= 0; index-- {
		entry := manifest.Tree.Entries[index]
		if entry.Kind != "file" {
			if err := applyToolMetadata(filepath.Join(tree, entry.Path), entry); err != nil {
				return err
			}
		} else if generationObjectFile(manifest.Installs, entry.Path) == "" {
			if err := applyToolMetadata(filepath.Join(tree, entry.Path), entry); err != nil {
				return err
			}
		}
	}
	expected, err := json.Marshal(manifest.Tree)
	if err != nil {
		return err
	}
	if err := verifyToolGenerationTree(ctx, tree, expected, maxBytes); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(stage, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return syncToolDirectory(stage)
}

func generationObjectFile(installs []ToolGenerationInstall, path string) string {
	for _, install := range installs {
		relative, err := filepath.Rel(install.Path, path)
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, "../") {
			return filepath.Join(install.ObjectID, "tree", relative)
		}
	}
	return ""
}

func linkToolGenerationEntry(root, tree string, installs []ToolGenerationInstall, entry toolEntry) error {
	target := filepath.Join(tree, entry.Path)
	switch entry.Kind {
	case "directory":
		return os.Mkdir(target, 0755)
	case "symlink":
		return os.Symlink(entry.Link, target)
	case "file":
		objectFile := generationObjectFile(installs, entry.Path)
		if objectFile != "" {
			return os.Link(filepath.Join(root, objectFile), target)
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		syncErr, closeErr := file.Sync(), file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	default:
		return fmt.Errorf("unsupported tool generation entry")
	}
}

func verifyToolGeneration(ctx context.Context, object string, expected []byte, tree toolManifest, maxBytes int64) error {
	info, err := os.Lstat(object)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("tool generation is not a closed directory")
	}
	path := filepath.Join(object, "manifest.json")
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > toolManifestLimit {
		return fmt.Errorf("tool generation manifest is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, expected) {
		return fmt.Errorf("tool generation manifest differs; existing generation preserved")
	}
	treeData, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	return verifyToolGenerationTree(ctx, filepath.Join(object, "tree"), treeData, maxBytes)
}

func verifyToolGenerationTree(ctx context.Context, tree string, expected []byte, maxBytes int64) error {
	audit, _, err := scanToolTree(ctx, tree, "generation-v1", maxBytes)
	if err != nil {
		return err
	}
	data, err := json.Marshal(audit)
	if err != nil || !bytes.Equal(data, expected) {
		return fmt.Errorf("tool generation payload differs; existing data preserved")
	}
	return nil
}
