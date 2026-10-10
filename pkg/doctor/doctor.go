// Package doctor reports, without starting anything, whether this host can
// run a shared act engine and its runs.
package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/nektos/act/pkg/serve"
)

// SchemaVersion versions the JSON report.
const SchemaVersion = 1

// Status is one check's outcome.
type Status string

// Check outcomes.
const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one finding with a stable ID.
type Check struct {
	ID          string `json:"id"`
	Required    bool   `json:"required"`
	Status      Status `json:"status"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is the whole bounded result.
type Report struct {
	SchemaVersion int     `json:"schema_version"`
	OK            bool    `json:"ok"`
	Checks        []Check `json:"checks"`
}

// DockerInfo is what doctor reads from the Docker daemon.
type DockerInfo struct {
	CgroupVersion string
	MemTotal      int64
	DockerRootDir string
	Rootless      bool
}

// Probes are doctor's only ways to look at the host; all are read-only.
type Probes struct {
	Ping      func(ctx context.Context) error
	Info      func(ctx context.Context) (DockerInfo, error)
	ReadFile  func(path string) ([]byte, error)
	FreeBytes func(path string) (uint64, error)
	Serve     func(ctx context.Context) (serve.Health, error)
}

// Options are the thresholds headroom is judged against.
type Options struct {
	MinDiskBytes   uint64
	MinMemoryBytes int64
}

const cgroupRoot = "/sys/fs/cgroup"

var runControllers = []string{"cpu", "memory", "pids"}

func missing(have string, want []string) []string {
	fields := map[string]bool{}
	for _, f := range strings.Fields(have) {
		fields[f] = true
	}
	var out []string
	for _, w := range want {
		if !fields[w] {
			out = append(out, w)
		}
	}
	return out
}

func gib(b float64) string { return fmt.Sprintf("%.1f GiB", b/(1<<30)) }

// Run performs every check and returns the report.
func Run(ctx context.Context, p Probes, opts Options) Report {
	var checks []Check
	add := func(c ...Check) { checks = append(checks, c...) }

	dockerOK := p.Ping(ctx) == nil
	var info DockerInfo
	var infoErr error
	if dockerOK {
		info, infoErr = p.Info(ctx)
		dockerOK = infoErr == nil
	}
	if dockerOK {
		add(Check{ID: "docker.reachable", Required: true, Status: Pass, Summary: "the Docker daemon answers"})
	} else {
		add(Check{ID: "docker.reachable", Required: true, Status: Fail, Summary: "the Docker daemon does not answer",
			Remediation: "start Docker, or set DOCKER_HOST to a reachable daemon"})
	}
	add(dockerChecks(dockerOK, info, opts, p)...)
	add(cgroupChecks(p)...)
	add(serveCheck(ctx, p))

	ok := true
	for _, c := range checks {
		if c.Required && c.Status == Fail {
			ok = false
		}
	}
	return Report{SchemaVersion: SchemaVersion, OK: ok, Checks: checks}
}
