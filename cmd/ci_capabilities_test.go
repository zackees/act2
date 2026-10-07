package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCICapabilitiesWithoutExecution(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	input := &Input{workdir: filepath.Join(t.TempDir(), "absent"), workflowsPath: "absent.yml"}
	root := createRootCommand(context.Background(), input, "0.2.89-act2.test")
	output := &bytes.Buffer{}
	root.SetOut(output)
	root.SetArgs([]string{"--ci-capabilities", "--json", "--ci-output", "precheck/precheck:plan"})
	if !assert.NoError(t, root.Execute()) {
		return
	}
	var report struct {
		SchemaVersion int      `json:"schema_version"`
		Producer      string   `json:"producer"`
		Version       string   `json:"version"`
		Capabilities  []string `json:"capabilities"`
	}
	assert.NoError(t, json.Unmarshal(output.Bytes(), &report), "stdout must contain exactly one JSON document")
	assert.Equal(t, 1, report.SchemaVersion)
	assert.Equal(t, "act2", report.Producer)
	assert.Equal(t, "0.2.89-act2.test", report.Version)
	assert.Equal(t, []string{"precheck/precheck:plan"}, input.ciOutputs)
	assert.ElementsMatch(t, []string{"qualified-job-identity-v1", "step-stage-result-v1", "selected-job-outputs-v1", "cache-exact-delete-v1"}, report.Capabilities)
}

type rejectedCapabilityWriter struct{}

func (rejectedCapabilityWriter) Write([]byte) (int, error) {
	return 0, errors.New("capability output refused")
}

func TestCICapabilitiesOutputFailure(t *testing.T) {
	assert.ErrorContains(t, writeCICapabilities(rejectedCapabilityWriter{}, "candidate"), "capability output refused")
}
