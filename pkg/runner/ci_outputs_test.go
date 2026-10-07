package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/assert"
)

func TestCIOutputsAreQualifiedOptInAndBounded(t *testing.T) {
	for _, test := range []struct {
		name      string
		selectors []string
		value     string
		result    string
		secret    string
		mask      string
		cleanup   bool
		cancelled bool
		jobCancel bool
		nested    bool
		want      string
		errorCode string
	}{
		{name: "requested", selectors: []string{"job1:matrix"}, value: "[]", result: "success", want: "[]"},
		{name: "nested", selectors: []string{"caller/job1:matrix"}, value: "[]", result: "success", want: "[]", nested: true},
		{name: "leaf-not-caller", selectors: []string{"job1:matrix"}, value: "[]", result: "success", nested: true},
		{name: "empty", selectors: []string{"job1:matrix"}, result: "success", want: ""},
		{name: "default", value: "[]", result: "success"},
		{name: "other-job", selectors: []string{"other:matrix"}, value: "[]", result: "success"},
		{name: "failed", selectors: []string{"job1:matrix"}, value: "[]", result: "failure"},
		{name: "cleanup", selectors: []string{"job1:matrix"}, value: "[]", result: "success", cleanup: true},
		{name: "cancelled-job-live-context", selectors: []string{"job1:matrix"}, value: "[]", result: "success", cancelled: true},
		{name: "cancelled-job-context", selectors: []string{"job1:matrix"}, value: "[]", result: "success", jobCancel: true},
		{name: "secret", selectors: []string{"job1:matrix"}, value: "before-private-after", result: "success", secret: "private", errorCode: "masked-output"},
		{name: "runtime-mask", selectors: []string{"job1:matrix"}, value: "before-private-after", result: "success", mask: "private", errorCode: "masked-output"},
		{name: "oversize", selectors: []string{"job1:matrix"}, value: strings.Repeat("x", 65537), result: "success", errorCode: "output-limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{"job1": createJob(t, "runs-on: ubuntu-latest", "")})
			rc.Config.JSONLogger = true
			rc.Config.CIOutputs = test.selectors
			rc.Config.Secrets = map[string]string{"test": test.secret}
			rc.Config.InsecureSecrets = true // Qualification still refuses masked values.
			rc.Masks = []string{test.mask}
			rc.Run.Job().Outputs = map[string]string{"matrix": test.value, "unrequested": "never-publish"}
			rc.ciOutputExpressions = map[string]string{"matrix": test.value, "unrequested": "never-publish"}
			rc.Run.Job().Result = test.result
			rc.Cancelled = test.cancelled
			if test.nested {
				rc.caller = &caller{runContext: &RunContext{Run: &model.Run{JobID: "caller",
					Workflow: &model.Workflow{Jobs: map[string]*model.Job{"caller": createJob(t, "runs-on: ubuntu-latest", "")}}}}}
			}
			if test.cleanup {
				rc.cleanupError = errors.New("cleanup failed")
			}
			factory := &skippedJobLoggerFactory{}
			ctx := WithJobLoggerFactory(context.Background(), factory)
			if test.jobCancel {
				cancelCtx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = common.WithJobCancelContext(ctx, cancelCtx)
			}
			_ = runPlannedJob(ctx, rc, nil, 0, func(current *RunContext) (common.Executor, error) {
				return func(ctx context.Context) error {
					if err := current.interpolateOutputs()(ctx); err != nil {
						return err
					}
					setJobOutputs(ctx, current)
					return nil
				}, nil
			})
			if test.want == "" && test.name != "empty" && test.errorCode == "" {
				assert.Empty(t, factory.buffer.String())
				return
			}
			var event struct {
				Schema   int                    `json:"ciOutputSchema"`
				Outputs  map[string]string      `json:"jobOutputs"`
				Error    string                 `json:"jobOutputsError"`
				Identity []jobIdentityComponent `json:"jobIdentity"`
			}
			if assert.NoError(t, json.Unmarshal(factory.buffer.Bytes(), &event)) {
				assert.Equal(t, 1, event.Schema)
				assert.Equal(t, rc.jobIdentity(), event.Identity)
				assert.Equal(t, test.errorCode, event.Error)
				if test.errorCode == "" {
					assert.Equal(t, map[string]string{"matrix": test.want}, event.Outputs)
				} else {
					assert.Empty(t, event.Outputs)
				}
			}
			assert.NotContains(t, factory.buffer.String(), "never-publish")
			assert.NotContains(t, factory.buffer.String(), "before-private-after")
		})
	}
}

func TestCIOutputSelectorsRefuseBeforeRunnerConfiguration(t *testing.T) {
	for _, selectors := range [][]string{{"missing-colon"}, {"job:output", "job:output"},
		{"job:output/other"}, {"job::output"}, {"/job:output"}, {strings.Repeat("a", 1025) + ":output"}} {
		_, err := New(&Config{JSONLogger: true, CIOutputs: selectors, EventPath: "/nonexistent/event.json"})
		assert.ErrorContains(t, err, "CI output selector")
	}
	_, err := New(&Config{CIOutputs: []string{"job:output"}})
	assert.ErrorContains(t, err, "requires --json")
	assert.NoError(t, validateCIOutputSelectors(&Config{JSONLogger: true, CIOutputs: []string{"caller/job:output"}}))
}

func TestCIOutputsKeepEachMatrixLegsOriginalExpressions(t *testing.T) {
	job := createJob(t, "runs-on: ubuntu-latest", "")
	job.Outputs = map[string]string{"matrix": "${{ matrix.lane }}"}
	run := &model.Run{JobID: "job1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job1": job}}}
	runner := &runnerImpl{config: &Config{JSONLogger: true, CIOutputs: []string{"job1:matrix"}}}
	left := runner.newRunContext(context.Background(), run, map[string]interface{}{"lane": "left"})
	right := runner.newRunContext(context.Background(), run, map[string]interface{}{"lane": "right"})
	job.Result = "success"
	for _, rc := range []*RunContext{left, right} {
		factory := &skippedJobLoggerFactory{}
		ctx := WithJobLoggerFactory(context.Background(), factory)
		assert.NoError(t, runPlannedJob(ctx, rc, rc.Matrix, 0, func(current *RunContext) (common.Executor, error) {
			return func(ctx context.Context) error {
				if err := current.interpolateOutputs()(ctx); err != nil {
					return err
				}
				setJobOutputs(ctx, current)
				return nil
			}, nil
		}))
		var event struct {
			Outputs  map[string]string      `json:"jobOutputs"`
			Identity []jobIdentityComponent `json:"jobIdentity"`
		}
		assert.NoError(t, json.Unmarshal(factory.buffer.Bytes(), &event))
		assert.Equal(t, rc.Matrix["lane"], event.Outputs["matrix"])
		assert.Equal(t, rc.jobIdentity(), event.Identity)
	}
}
