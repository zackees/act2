package artifactcache

import "context"

// RetireToolObject removes one verified immutable object only after a complete
// bounded inventory proves no retained generation refers to it. Caller determines
// age/pressure eligibility. Unknown reference state refuses deletion.
func RetireToolObject(ctx context.Context, root, id string, maxBytes int64, maxGenerations int) error {
	return retireToolObject(ctx, root, id, maxBytes, maxGenerations)
}
