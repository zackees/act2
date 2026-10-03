package runner

import (
	"context"
	"path/filepath"
	"testing"
)

// act2#9: shell and JavaScript actions share an ordinary hosted runner user;
// explicit job containers retain their own user. Include real tool installers.
func TestHostedRunnerActionPath(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker action-path integration")
	}
	job := TestJobFileInfo{
		workdir:      filepath.Join(workdir, "hosted-user"),
		workflowPath: "parity.yml",
		eventName:    "workflow_dispatch",
		platforms:    map[string]string{"ubuntu-24.04": "docker.io/catthehacker/ubuntu@sha256:4f2d5083a9d10d018c1c511eb8665cd480553c11975e78fd903a46daa830768b"},
	}
	job.runTest(context.Background(), t, &Config{})
}
