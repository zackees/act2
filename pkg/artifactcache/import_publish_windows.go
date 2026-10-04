package artifactcache

import "golang.org/x/sys/windows"

// Windows does not support the Unix read-only directory fsync path. The
// native same-volume move requests write-through and never replaces a target.
// Archive files and metadata have already been synced and closed.
func publishImportStage(stage, destination, _ string) (bool, error) {
	from, err := windows.UTF16PtrFromString(stage)
	if err != nil {
		return false, err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return false, err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return false, err
	}
	return true, nil
}
