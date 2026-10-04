//go:build linux

package artifactcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const toolRecoveryPinDirectory = ".tool-recovery-pins-v1"
const toolRecoveryPinLimit = 10000
const toolRecoveryPinBytes = 1024
const toolRecoveryPinLifetime = 24 * time.Hour

func canonicalRecoveryID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == id
}

func (pin ToolRecoveryPin) validate() error {
	if pin.SchemaVersion != 1 || !canonicalRecoveryID(pin.Owner) || !canonicalRecoveryID(pin.Generation) || pin.CreatedAt.IsZero() || pin.ExpiresAt.IsZero() || !pin.ExpiresAt.After(pin.CreatedAt) || pin.ExpiresAt.Sub(pin.CreatedAt) > toolRecoveryPinLifetime {
		return fmt.Errorf("recovery pin identity or finite lifetime is invalid")
	}
	return nil
}

// The caller holds the original catalog writer. Malformed, oversized or
// ambiguous records stop retirement; absence alone means no reservations.
func readToolRecoveryPinRecords(ctx context.Context, root string, now time.Time) (map[string]ToolRecoveryPin, error) {
	records := make(map[string]ToolRecoveryPin)
	verifiedGenerations := make(map[string]bool)
	directory := filepath.Join(root, toolRecoveryPinDirectory)
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return records, nil
	}
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("recovery pin namespace is invalid")
	}
	rootMount, err := toolMountID(root)
	if err != nil {
		return nil, err
	}
	pinMount, err := toolMountID(directory)
	if err != nil || rootMount != pinMount {
		return nil, fmt.Errorf("recovery pin namespace crosses mount boundary")
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(toolRecoveryPinLimit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > toolRecoveryPinLimit {
		return nil, fmt.Errorf("recovery pin inventory exceeds bound")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner := strings.TrimSuffix(entry.Name(), ".json")
		if !canonicalRecoveryID(owner) || entry.Name() != owner+".json" {
			return nil, fmt.Errorf("recovery pin filename is invalid")
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > toolRecoveryPinBytes {
			return nil, fmt.Errorf("recovery pin is not a bounded regular record")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, toolRecoveryPinBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > toolRecoveryPinBytes {
			return nil, fmt.Errorf("recovery pin read failed or exceeded bound")
		}
		var pin ToolRecoveryPin
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&pin); err != nil {
			return nil, fmt.Errorf("recovery pin record is invalid: %w", err)
		}
		canonical, err := json.Marshal(pin)
		if err != nil || !bytes.Equal(canonical, data) || pin.Owner != owner {
			return nil, fmt.Errorf("recovery pin is not canonical or owner-bound")
		}
		if err := pin.validate(); err != nil {
			return nil, err
		}
		if pin.CreatedAt.After(now) {
			return nil, fmt.Errorf("recovery pin creation is in the future")
		}
		if pin.ExpiresAt.After(now) && !verifiedGenerations[pin.Generation] {
			if _, _, err := readToolGenerationManifest(filepath.Join(root, toolGenerationDirectory, pin.Generation), pin.Generation); err != nil {
				return nil, fmt.Errorf("active recovery pin lower is invalid: %w", err)
			}
			verifiedGenerations[pin.Generation] = true

		}
		records[pin.Owner] = pin
	}
	return records, nil
}

func readToolRecoveryPins(ctx context.Context, root string, now time.Time) (map[string]bool, error) {
	records, err := readToolRecoveryPinRecords(ctx, root, now)
	if err != nil {
		return nil, err
	}
	protected := make(map[string]bool)
	for _, pin := range records {
		if pin.ExpiresAt.After(now) {
			protected[pin.Generation] = true
		}
	}
	return protected, nil
}
