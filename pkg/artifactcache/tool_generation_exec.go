package artifactcache

import "context"

// ExecWithToolGeneration validates a closed generation under catalog protection,
// holds its original reader inode and replaces this process with command.
// Linux only. Call from a dedicated engine init process: all of this process's
// other threads and Go state disappear on success. The inherited descriptor is
// reported in BOSN_TOOL_GENERATION_LEASE_FD. The command must keep it open for
// the complete engine lifetime and expose the generation tree only read-only.
func ExecWithToolGeneration(ctx context.Context, root, id string, maxBytes int64, command []string) error {
	return execWithToolGeneration(ctx, root, id, maxBytes, command)
}
