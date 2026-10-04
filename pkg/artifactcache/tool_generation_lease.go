package artifactcache

import "context"

// ToolGenerationLease protects a closed generation for the caller's complete
// engine lifetime. Close only after all mounts and readers have gone away.
// Process death releases the OS descriptor; engine integration must keep the
// holder alive as long as the engine, including uncertain cleanup states.
type ToolGenerationLease interface{ Close() error }

// AcquireToolGenerationLease acquires the original reader under catalog writer
// exclusion, then validates payload with that reader protecting the generation.
// Expensive hashing does not hold the catalog mutex. Failed validation releases
// the reader and never admits unverified payload.
func AcquireToolGenerationLease(ctx context.Context, root, id string, maxBytes int64) (ToolGenerationLease, error) {
	return acquireToolGenerationLease(ctx, root, id, maxBytes)
}
