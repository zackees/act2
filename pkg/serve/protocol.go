// Package serve runs act as a long-lived service inside a shared engine:
// it admits runs, gives each its own cgroup, network, work tree and ports,
// runs act inside that scope, and removes everything a run left behind.
package serve

import (
	"errors"
	"fmt"
	"strings"
)

// ProtocolVersion is the serve API version a client and server agree on.
const ProtocolVersion = 1

// DefaultSocket is where the server listens unless told otherwise.
const DefaultSocket = "/run/act2/serve.sock"

// Limits are written to a run's cgroup.
type Limits struct {
	MemoryBytes uint64 `json:"memory_bytes"`
	NanoCPUs    uint64 `json:"nano_cpus"`
	Pids        uint64 `json:"pids"`
}

// AdmitRequest asks for a run to be admitted.
type AdmitRequest struct {
	RunID  string `json:"run_id"`
	Limits Limits `json:"limits"`
}

// Scope is everything one admitted run owns inside the engine.
type Scope struct {
	RunID        string `json:"run_id"`
	Key          string `json:"key"`
	Slot         int    `json:"slot"`
	Cgroup       string `json:"cgroup"`
	Network      string `json:"network"`
	Work         string `json:"work"`
	ArtifactPort int    `json:"artifact_port"`
	CachePort    int    `json:"cache_port"`
	Limits       Limits `json:"limits"`
}

// ExecRequest starts act in an admitted run's scope.
type ExecRequest struct {
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env,omitempty"`
	DeadlineSecs int64             `json:"deadline_secs,omitempty"`
}

// Exit ends an exec stream.
type Exit struct {
	Code     int    `json:"code"`
	Signal   string `json:"signal,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Frame is one NDJSON line of an exec stream: output, or the final exit.
type Frame struct {
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Exit   *Exit  `json:"exit,omitempty"`
}

// Health describes a running server.
type Health struct {
	Protocol int      `json:"protocol"`
	Version  string   `json:"version"`
	MaxRuns  int      `json:"max_runs"`
	Runs     []string `json:"runs"`
}

// ErrorBody is every non-2xx response.
type ErrorBody struct {
	Error string `json:"error"`
}

// RunKey is the short form of a canonical UUID run ID: its first twelve hex
// digits, short enough for socket paths and network names.
func RunKey(runID string) (string, error) {
	if len(runID) != 36 {
		return "", fmt.Errorf("run ID %q is not a canonical UUID", runID)
	}
	for i, c := range runID {
		dash := i == 8 || i == 13 || i == 18 || i == 23
		if dash != (c == '-') || (!dash && !strings.ContainsRune("0123456789abcdef", c)) {
			return "", fmt.Errorf("run ID %q is not a canonical UUID", runID)
		}
	}
	return strings.ReplaceAll(runID, "-", "")[:12], nil
}

// Validate refuses limits that would leave a run unbounded or unusable.
func (l Limits) Validate() error {
	switch {
	case l.MemoryBytes < 64<<20:
		return errors.New("memory_bytes must be at least 64 MiB")
	case l.NanoCPUs < 10_000_000:
		return errors.New("nano_cpus must be at least 0.01 CPU")
	case l.Pids < 16:
		return errors.New("pids must be at least 16")
	}
	return nil
}

// CPUMax is the cgroup v2 cpu.max value: quota per 100ms period.
func (l Limits) CPUMax() string {
	return fmt.Sprintf("%d 100000", l.NanoCPUs/10_000)
}
