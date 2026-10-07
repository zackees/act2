package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	log "github.com/sirupsen/logrus"
	assert "github.com/stretchr/testify/assert"
)

type skippedJobLoggerFactory struct {
	buffer bytes.Buffer
}

func (factory *skippedJobLoggerFactory) WithJobLogger() *log.Logger {
	logger := log.New()
	logger.SetOutput(&factory.buffer)
	logger.SetLevel(log.InfoLevel)
	logger.SetFormatter(&log.JSONFormatter{})
	return logger
}

func TestSkippedJobPublishesQualifiedResultAtNormalLogLevel(t *testing.T) {
	for _, nested := range []bool{false, true} {
		rc := createIfTestRunContext(map[string]*model.Job{
			"job1": createJob(t, "runs-on: ubuntu-latest\nif: false", ""),
		})
		rc.Config.JSONLogger = true
		rc.Matrix = map[string]interface{}{"shard": 1}
		path := []string{"job1"}
		if nested {
			rc.caller = &caller{runContext: &RunContext{Run: &model.Run{JobID: "caller"}}}
			path = []string{"caller", "job1"}
		}
		factory := &skippedJobLoggerFactory{}
		ctx := WithJobLoggerFactory(context.Background(), factory)
		err := runPlannedJob(ctx, rc, rc.Matrix, 0, func(current *RunContext) (common.Executor, error) {
			return func(ctx context.Context) error {
				enabled, err := current.isEnabled(ctx)
				assert.False(t, enabled)
				return err
			}, nil
		})
		assert.NoError(t, err)
		var wire struct {
			Result   string                 `json:"jobResult"`
			ID       string                 `json:"jobID"`
			Path     []string               `json:"jobPath"`
			Identity []jobIdentityComponent `json:"jobIdentity"`
		}
		if assert.NoError(t, json.Unmarshal(factory.buffer.Bytes(), &wire)) {
			assert.Equal(t, "skipped", wire.Result)
			assert.Equal(t, "job1", wire.ID)
			assert.Equal(t, path, wire.Path)
			assert.Equal(t, rc.jobIdentity(), wire.Identity)
		}
	}
}
