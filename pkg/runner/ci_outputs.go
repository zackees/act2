package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nektos/act/pkg/common"
	log "github.com/sirupsen/logrus"
)

const maxCIOutputBytes = 64 * 1024

var ciOutputSelector = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(/[A-Za-z_][A-Za-z0-9_-]*)*:[A-Za-z_][A-Za-z0-9_-]*$`)

func validateCIOutputSelectors(config *Config) error {
	if len(config.CIOutputs) > 256 || (len(config.CIOutputs) > 0 && !config.JSONLogger) {
		return fmt.Errorf("CI output evidence requires --json and at most 256 selectors")
	}
	seen := make(map[string]bool)
	for _, selector := range config.CIOutputs {
		if len(selector) > 1024 || !ciOutputSelector.MatchString(selector) || seen[selector] {
			return fmt.Errorf("CI output selector must be a distinct job-path:output")
		}
		seen[selector] = true
	}
	return nil
}

// Publish only explicitly requested, interpolated outputs of this concrete
// successful job. The event is evidence, not an attestation or a planner.
func publishCIOutputs(ctx context.Context, rc *RunContext) {
	if !rc.Config.JSONLogger || len(rc.Config.CIOutputs) == 0 || !ciOutputExecutionPassed(ctx, rc) {
		return
	}
	path := rc.jobPath()
	if len(path) == 0 {
		return
	}
	prefix := strings.Join(path, "/") + ":"
	outputs := make(map[string]string)
	problem := ""
	for _, selector := range rc.Config.CIOutputs {
		if !strings.HasPrefix(selector, prefix) {
			continue
		}
		name := strings.TrimPrefix(selector, prefix)
		value, found := rc.ciOutputValues[name]
		if !found {
			problem = "missing-output"
			break
		}
		if ciOutputMasked(ctx, rc.Config, value) {
			problem = "masked-output"
			break
		}
		outputs[name] = value
	}
	if len(outputs) == 0 && problem == "" {
		return
	}
	encoded, err := json.Marshal(outputs)
	if err != nil || len(encoded) > maxCIOutputBytes || len(outputs) > 256 {
		problem = "output-limit"
	}
	fields := log.Fields{"ciOutputSchema": 1}
	if problem != "" {
		fields["jobOutputsError"] = problem
	} else {
		fields["jobOutputs"] = outputs
	}
	common.Logger(ctx).WithFields(fields).Info("CI output evidence")
}

func ciOutputExecutionPassed(ctx context.Context, rc *RunContext) bool {
	cancelContext := common.JobCancelContext(ctx)
	return rc.Run.Job().Result == "success" && rc.cleanupError == nil && !rc.Cancelled && ctx.Err() == nil &&
		(cancelContext == nil || cancelContext.Err() == nil) && !common.Dryrun(ctx)
}

func ciOutputMasked(ctx context.Context, config *Config, value string) bool {
	contains := func(secret string) bool { return secret != "" && strings.Contains(value, secret) }
	if contains(config.Token) {
		return true
	}
	for _, secret := range config.Secrets {
		if contains(secret) {
			return true
		}
	}
	for _, mask := range *Masks(ctx) {
		if contains(mask) {
			return true
		}
	}
	return false
}
