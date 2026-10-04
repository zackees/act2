package artifactcache

import (
	"fmt"
	"math"
)

type importSpaceProbe func(string) (uint64, error)

// The estimate includes metadata headroom and an allocation cushion per
// archive. This is admission evidence, not a reservation against other writers.
const importMetadataHeadroom = uint64(auditMaxMetadataBytes)
const importArchiveHeadroom = uint64(64 * 1024)

func checkImportHeadroom(stage string, caches []*Cache, report *ImportReport, probe importSpaceProbe) error {
	required := importMetadataHeadroom
	for _, cache := range caches {
		if cache.Size < 0 {
			return fmt.Errorf("import archive has an unknown size")
		}
		size := uint64(cache.Size)
		if size > math.MaxUint64-importArchiveHeadroom || required > math.MaxUint64-size-importArchiveHeadroom {
			return fmt.Errorf("import headroom size overflow")
		}
		required += size + importArchiveHeadroom
	}
	report.RequiredAdditionalBytes = &required
	available, err := probe(stage)
	if err != nil {
		return fmt.Errorf("destination free space is unknown: %w", err)
	}
	report.AvailableDestinationBytes = &available
	if available < required {
		return fmt.Errorf("insufficient destination headroom: %d available bytes, %d estimated additional bytes", available, required)
	}
	return nil
}
