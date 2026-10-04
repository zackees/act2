//go:build !linux && !darwin && !windows

package artifactcache

import "fmt"

func availableImportSpace(string) (uint64, error) {
	return 0, fmt.Errorf("free-space measurement is unavailable on this platform")
}
