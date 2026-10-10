package serve

import (
	"context"
	"os/exec"
)

// Isolator creates, enters and removes a run's scope. The Linux isolator
// uses cgroup v2 and the engine's Docker; tests use a fake.
type Isolator interface {
	// Open creates the scope's cgroup, network and work tree. Idempotent.
	Open(ctx context.Context, scope Scope) error
	// Prepare places cmd in the scope's cgroup when it starts. The returned
	// function runs once the command has started.
	Prepare(cmd *exec.Cmd, scope Scope) (func(), error)
	// Kill stops every process act started in the scope.
	Kill(scope Scope) error
	// Close removes everything the scope owns and proves none is left.
	Close(ctx context.Context, scope Scope) error
	// Reap removes every scope a previous server left behind.
	Reap(ctx context.Context) error
}
