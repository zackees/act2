//go:build !windows

package artifactcache

import "os"

// A successful rename stays published even if parent-directory sync fails.
func publishImportStage(stage, destination, root string) (bool, error) {
	if err := os.Rename(stage, destination); err != nil {
		return false, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return true, err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return true, syncErr
	}
	return true, closeErr
}
