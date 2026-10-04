//go:build !windows

package artifactcache

import "os"

// A successful rename stays published even if parent-directory sync fails.
func publishImportStage(stage, destination, root string) (bool, error) {
	// #nosec G703 -- generated private stage and validated direct-child destination.
	if err := os.Rename(stage, destination); err != nil {
		return false, err
	}
	directory, err := os.Open(root) // #nosec G703 -- validated caller-selected cohort root.
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
