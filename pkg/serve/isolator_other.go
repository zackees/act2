//go:build !linux

package serve

import "errors"

// NewIsolator is only available on Linux: runs are scoped with cgroup v2.
func NewIsolator(Config) (Isolator, error) {
	return nil, errors.New("act serve needs Linux with cgroup v2")
}
