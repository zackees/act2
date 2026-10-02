package container

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// `runner.environment` mirrors GitHub: act's runner images emulate a
// GitHub-hosted runner, and host mode is a self-hosted one.
func TestRunnerContextEnvironment(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "self-hosted", (&HostEnvironment{}).GetRunnerContext(ctx)["environment"])
	assert.Equal(t, "github-hosted", (&LinuxContainerEnvironmentExtensions{}).GetRunnerContext(ctx)["environment"])
}
