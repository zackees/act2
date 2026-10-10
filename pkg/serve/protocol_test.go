package serve

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunKeyAcceptsOnlyCanonicalUUIDs(t *testing.T) {
	key, err := RunKey("0a1b2c3d-4e5f-4a6b-8c7d-000000000001")
	assert.NoError(t, err)
	assert.Equal(t, "0a1b2c3d4e5f", key)
	for _, bad := range []string{"", "not-a-run", "0A1B2C3D-4E5F-4A6B-8C7D-000000000001", "0a1b2c3d-4e5f-4a6b-8c7d-00000000000;", "0a1b2c3d4e5f04a6b08c7d0000000000001"} {
		_, err := RunKey(bad)
		assert.Error(t, err, bad)
	}
}

func TestLimitsRefuseUnboundedRuns(t *testing.T) {
	assert.NoError(t, Limits{MemoryBytes: 1 << 30, NanoCPUs: 2_000_000_000, Pids: 512}.Validate())
	assert.Error(t, Limits{NanoCPUs: 2_000_000_000, Pids: 512}.Validate())
	assert.Error(t, Limits{MemoryBytes: 1 << 30, Pids: 512}.Validate())
	assert.Error(t, Limits{MemoryBytes: 1 << 30, NanoCPUs: 2_000_000_000}.Validate())
	assert.Equal(t, "200000 100000", Limits{NanoCPUs: 2_000_000_000}.CPUMax())
}

func TestConfigKeepsPortsInRange(t *testing.T) {
	ok := Config{RunLabel: "l", ScopePrefix: "p-", WorkRoot: "/w", PortBase: 40000, MaxRuns: 256}
	assert.NoError(t, ok.Validate())
	high := ok
	high.MaxRuns = 20000
	assert.Error(t, high.Validate())
	relative := ok
	relative.WorkRoot = "w"
	assert.Error(t, relative.Validate())
}
