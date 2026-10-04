//go:build !linux

package artifactcache

import (
	"context"
	"fmt"
)

func retireToolObject(context.Context, string, string, int64, int) error {
	return fmt.Errorf("tool object retirement requires Linux")
}
