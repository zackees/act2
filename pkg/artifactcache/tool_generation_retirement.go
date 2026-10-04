package artifactcache

import "context"

// RetireToolGeneration removes one verified unselected generation after excluding
// live readers. Callers must establish age/pressure eligibility separately. Shared
// objects remain intact. Errors can follow partial deletion; never infer reclaimed
// bytes from success or an error without a fresh accounting observation.
func RetireToolGeneration(ctx context.Context, root, id string, maxBytes int64) error {
	return retireToolGeneration(ctx, root, id, maxBytes)
}
