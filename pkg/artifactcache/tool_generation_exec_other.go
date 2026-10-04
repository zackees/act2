//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func execWithToolGeneration(context.Context, string, string, int64, []string) error {
	return fmt.Errorf("tool generation exec is supported only on Linux")
}
