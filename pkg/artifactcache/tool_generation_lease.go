package artifactcache

import "context"

// ToolGenerationLease protects a closed generation for the caller's complete
// engine lifetime. Close only after all mounts and readers have gone away.
// Process death releases the OS descriptor; engine integration must keep the
// holder alive as long as the engine, including uncertain cleanup states.
type ToolGenerationLease interface{ Close() error }

// AcquireToolGenerationLease validates and locks a published generation while
// the catalog writer lock prevents coordinated retirement or admission races.
func AcquireToolGenerationLease(ctx context.Context, root, id string, maxBytes int64) (ToolGenerationLease, error) {
	return acquireToolGenerationLease(ctx, root, id, maxBytes)
}
