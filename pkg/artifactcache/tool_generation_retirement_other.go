//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func retireToolGeneration(context.Context, string, string, int64) error {
	return fmt.Errorf("tool generation retirement requires Linux")
}
