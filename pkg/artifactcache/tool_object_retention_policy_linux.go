//go:build linux

package artifactcache

import (
	"context"
	"errors"
	"fmt"
)

func sweepToolObjects(ctx context.Context, root string, policy ToolRetentionPolicy, candidates []toolRetentionCandidate, report ToolRetentionReport) ToolRetentionReport {
	for _, candidate := range candidates {
		if !candidate.published.Before(policy.ExpireBefore) && *report.After.AllocatedBytes <= policy.MaxAllocatedBytes {
			continue
		}
		if err := ctx.Err(); err != nil {
			report.fail(err)
			break
		}
		err := retireToolObjectLocked(ctx, root, candidate.id, policy.MaxPayloadBytes, policy.MaxCandidates)
		if errors.Is(err, errToolObjectReferenced) {
			report.ProtectedObjects = append(report.ProtectedObjects, candidate.id)
			continue
		}
		if err == nil {
			report.RetiredObjects = append(report.RetiredObjects, candidate.id)
		} else {
			report.fail(err)
		}
		report.After = auditToolStoreUsageLocked(ctx, root, policy.MaxEntries)
		if report.After.Partial {
			report.fail(fmt.Errorf("post-object-retirement inventory incomplete: %s", report.After.Error))
			break
		}
	}
	if report.After.AllocatedBytes != nil {
		report.ProtectedOverflow = *report.After.AllocatedBytes > policy.MaxAllocatedBytes
	}
	return report
}
