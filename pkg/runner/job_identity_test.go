package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	log "github.com/sirupsen/logrus"
	assert "github.com/stretchr/testify/assert"
)

// Identical display names and leaf IDs still describe different executions.
// The caller IDs must remain available on the actual executor's logger.
func TestPlannedJobLoggerKeepsReusableCallerIdentity(t *testing.T) {
	for _, callerID := range []string{"maintenance", "validation"} {
		t.Run(callerID, func(t *testing.T) {
			root := &RunContext{
				Name: "same display",
				Run:  &model.Run{JobID: callerID, Workflow: &model.Workflow{Name: "root"}},
			}
			middle := &RunContext{
				Name:   "same display",
				Run:    &model.Run{JobID: "nested", Workflow: &model.Workflow{Name: "child"}},
				caller: &caller{runContext: root},
			}
			leaf := &RunContext{
				Name:   "same display",
				Config: &Config{JSONLogger: true},
				Run:    &model.Run{JobID: "cache-budget", Workflow: &model.Workflow{Name: "grandchild"}},
				caller: &caller{runContext: middle},
			}
			err := runPlannedJob(context.Background(), leaf, nil, 0,
				func(*RunContext) (common.Executor, error) {
					return func(ctx context.Context) error {
						entry, ok := common.Logger(ctx).(*log.Entry)
						if assert.True(t, ok, "planned jobs use structured log entries") {
							assert.Equal(t, []string{callerID, "nested", "cache-budget"}, entry.Data["jobPath"])
							assert.Equal(t, "cache-budget", entry.Data["jobID"])
							encoded, formatErr := entry.Logger.Formatter.Format(entry)
							if assert.NoError(t, formatErr) {
								var wire struct {
									Path []string `json:"jobPath"`
									ID   string   `json:"jobID"`
								}
								assert.NoError(t, json.Unmarshal(encoded, &wire))
								assert.Equal(t, []string{callerID, "nested", "cache-budget"}, wire.Path)
								assert.Equal(t, "cache-budget", wire.ID)
							}
						}
						return nil
					}, nil
				})
			assert.NoError(t, err)
		})
	}
}

func TestJobIdentityRejectsIncompleteAndCyclicCallers(t *testing.T) {
	root := &RunContext{Run: &model.Run{JobID: "root"}}
	assert.Equal(t, []string{"root"}, root.jobPath())
	root.caller = &caller{runContext: root}
	assert.Nil(t, root.jobPath())
	root.caller = &caller{}
	assert.Nil(t, root.jobPath())
	root.caller = &caller{runContext: &RunContext{}}
	assert.Nil(t, root.jobPath())
	root.caller = &caller{runContext: &RunContext{Run: &model.Run{}}}
	assert.Nil(t, root.jobPath())
}

func TestJobIdentityBoundsCallerDepth(t *testing.T) {
	root := &RunContext{Run: &model.Run{JobID: "root"}}
	for range 31 {
		root = &RunContext{Run: &model.Run{JobID: "nested"}, caller: &caller{runContext: root}}
	}
	assert.Len(t, root.jobPath(), 32)
	root = &RunContext{Run: &model.Run{JobID: "nested"}, caller: &caller{runContext: root}}
	assert.Nil(t, root.jobPath())
}
