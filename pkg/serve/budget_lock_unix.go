//go:build !windows

package serve

import (
	"os"
	"syscall"
)

// tryLock takes f's exclusive lock without waiting.
func tryLock(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
