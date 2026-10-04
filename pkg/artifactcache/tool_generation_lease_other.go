//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func acquireToolGenerationLease(_ context.Context, _, _ string, _ int64) (ToolGenerationLease, error) {
	return nil, fmt.Errorf("immutable tool generation leases require Linux")
}
