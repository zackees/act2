//go:build linux

package artifactcache

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bboltErrors "go.etcd.io/bbolt/errors"
)

type toolRetentionCandidate struct {
	id        string
	published time.Time
}

func retainToolStore(ctx context.Context, root string, policy ToolRetentionPolicy) (report ToolRetentionReport) {
	report.SchemaVersion = 1
	if policy.MaxAllocatedBytes <= 0 || policy.ExpireBefore.IsZero() || policy.MaxCandidates <= 0 || policy.MaxCandidates > 10000 || policy.MaxEntries <= 0 || policy.MaxEntries > 1000000 {
		report.fail(fmt.Errorf("retention requires positive allocation, explicit cutoff and bounded entries/candidates"))
		return report
	}
	guard := ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "retention", ObjectID: strings.Repeat("0", 64)}}}
	if _, err := validateToolGenerationSpec(root, guard, policy.MaxPayloadBytes); err != nil {
		report.fail(err)
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	catalog, err := prepareExistingToolSnapshotStore(root)
	if err != nil {
		report.fail(err)
		return report
	}
	defer catalog.Close()
	selection, err := readToolGenerationSelection(root)
	if err != nil {
		report.fail(err)
		return report
	}
	if _, err := verifySelectedToolGeneration(ctx, root, selection, policy.MaxPayloadBytes); err != nil {
		report.fail(err)
		return report
	}
	report.Before = auditToolStoreUsageLocked(ctx, root, policy.MaxEntries)
	report.After = report.Before
	if report.Before.Partial {
		report.fail(fmt.Errorf("retention inventory incomplete: %s", report.Before.Error))
		return report
	}
	candidates, err := toolRetentionCandidates(root, policy.MaxCandidates)
	if err != nil {
		report.fail(err)
		return report
	}
	return sweepToolGenerations(ctx, root, policy, selection, candidates, report)
}

func sweepToolGenerations(ctx context.Context, root string, policy ToolRetentionPolicy, selection ToolGenerationSelection, candidates []toolRetentionCandidate, report ToolRetentionReport) ToolRetentionReport {
	for _, candidate := range candidates {
		if candidate.id == selection.ID {
			report.ProtectedGenerations = append(report.ProtectedGenerations, candidate.id)
			continue
		}
		if !candidate.published.Before(policy.ExpireBefore) && *report.After.AllocatedBytes <= policy.MaxAllocatedBytes {
			continue
		}
		if err := ctx.Err(); err != nil {
			report.fail(err)
			break
		}
		err := retireToolGenerationLocked(ctx, root, candidate.id, policy.MaxPayloadBytes)
		if errors.Is(err, bboltErrors.ErrTimeout) {
			report.ProtectedGenerations = append(report.ProtectedGenerations, candidate.id)
			continue
		}
		if err == nil {
			report.RetiredGenerations = append(report.RetiredGenerations, candidate.id)
		} else {
			report.fail(err)
		}
		// Even failed deletion may have reclaimed some state. Never credit logical bytes.
		report.After = auditToolStoreUsageLocked(ctx, root, policy.MaxEntries)
		if report.After.Partial {
			report.fail(fmt.Errorf("post-retirement inventory incomplete: %s", report.After.Error))
			break
		}
	}
	if report.After.AllocatedBytes != nil {
		report.ProtectedOverflow = *report.After.AllocatedBytes > policy.MaxAllocatedBytes
	}
	return report
}

func toolRetentionCandidates(root string, bound int) ([]toolRetentionCandidate, error) {
	directory := filepath.Join(root, toolGenerationDirectory)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("generation namespace is invalid")
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(bound + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > bound {
		return nil, fmt.Errorf("generation candidate inventory exceeds bound")
	}
	candidates := make([]toolRetentionCandidate, 0, len(entries))
	for _, entry := range entries {
		decoded, err := hex.DecodeString(entry.Name())
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != entry.Name() || !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, toolRetentionCandidate{entry.Name(), info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].published.Equal(candidates[j].published) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].published.Before(candidates[j].published)
	})
	return candidates, nil
}
