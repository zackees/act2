package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/nektos/act/pkg/serve"
)

func dockerChecks(reachable bool, info DockerInfo, opts Options, p Probes) []Check {
	if !reachable {
		skipped := func(id string, required bool) Check {
			return Check{ID: id, Required: required, Status: Skip, Summary: "Docker is not reachable"}
		}
		return []Check{skipped("docker.cgroup_v2", true), skipped("docker.privileged", true),
			skipped("memory.headroom", false), skipped("disk.headroom", false)}
	}
	checks := make([]Check, 0, 4)
	if info.CgroupVersion == "2" {
		checks = append(checks, Check{ID: "docker.cgroup_v2", Required: true, Status: Pass, Summary: "Docker uses cgroup v2"})
	} else {
		checks = append(checks, Check{ID: "docker.cgroup_v2", Required: true, Status: Fail,
			Summary:     fmt.Sprintf("Docker reports cgroup version %q; per-run limits need v2", info.CgroupVersion),
			Remediation: "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)"})
	}
	if info.Rootless {
		checks = append(checks, Check{ID: "docker.privileged", Required: true, Status: Fail,
			Summary: "rootless Docker cannot run the privileged engine", Remediation: "use a rootful Docker daemon"})
	} else {
		checks = append(checks, Check{ID: "docker.privileged", Required: true, Status: Pass, Summary: "Docker is rootful, so the privileged engine can run"})
	}
	memory := Check{ID: "memory.headroom", Status: Pass, Summary: "Docker sees " + gib(float64(info.MemTotal))}
	if info.MemTotal < opts.MinMemoryBytes {
		memory.Status, memory.Remediation = Warn, "give Docker at least "+gib(float64(opts.MinMemoryBytes))
	}
	checks = append(checks, memory)
	return append(checks, diskCheck(info.DockerRootDir, opts, p))
}

func diskCheck(root string, opts Options, p Probes) Check {
	check := Check{ID: "disk.headroom"}
	free, err := p.FreeBytes(root)
	switch {
	case root == "" || err != nil:
		check.Status, check.Summary = Skip, fmt.Sprintf("Docker's data root %q is not visible from here", root)
	case free < opts.MinDiskBytes:
		check.Status, check.Summary = Warn, gib(float64(free))+" free under "+root
		check.Remediation = "free disk space; the engine and runner image need at least " + gib(float64(opts.MinDiskBytes))
	default:
		check.Status, check.Summary = Pass, gib(float64(free))+" free under "+root
	}
	return check
}

func cgroupChecks(p Probes) []Check {
	controllers, err := p.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		summary := "cannot read " + cgroupRoot + "/cgroup.controllers"
		if errors.Is(err, fs.ErrNotExist) {
			summary = "no cgroup v2 hierarchy at " + cgroupRoot
		}
		return []Check{
			{ID: "cgroup.controllers", Required: true, Status: Fail, Summary: summary, Remediation: "run on Linux with cgroup v2 mounted at " + cgroupRoot},
			{ID: "cgroup.delegation", Status: Skip, Summary: "no cgroup v2 hierarchy"},
		}
	}
	checks := make([]Check, 0, 2)
	if gone := missing(string(controllers), runControllers); len(gone) > 0 {
		checks = append(checks, Check{ID: "cgroup.controllers", Required: true, Status: Fail,
			Summary: "controllers not available: " + strings.Join(gone, " "), Remediation: "enable the cpu, memory and pids controllers"})
	} else {
		checks = append(checks, Check{ID: "cgroup.controllers", Required: true, Status: Pass, Summary: "cpu, memory and pids controllers are available"})
	}
	subtree, err := p.ReadFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"))
	if gone := missing(string(subtree), runControllers); err != nil || len(gone) > 0 {
		checks = append(checks, Check{ID: "cgroup.delegation", Status: Warn,
			Summary:     "cpu, memory and pids are not all delegated to child cgroups here",
			Remediation: "inside the engine, move processes out of the root cgroup and enable +cpu +memory +pids in cgroup.subtree_control"})
	} else {
		checks = append(checks, Check{ID: "cgroup.delegation", Status: Pass, Summary: "cpu, memory and pids are delegated to child cgroups"})
	}
	return checks
}

func serveCheck(ctx context.Context, p Probes) Check {
	health, err := p.Serve(ctx)
	if err != nil {
		return Check{ID: "serve.running", Status: Skip, Summary: "act serve is not running here"}
	}
	return Check{ID: "serve.running", Status: Pass,
		Summary: fmt.Sprintf("act serve %s (protocol %d): %d of %d runs admitted; %s", health.Version, health.Protocol, len(health.Runs), health.MaxRuns, serveState(health))}
}

// serveState summarizes what the server owns: the run proxies, the runner
// image, the tool cache and the shared cache budget.
func serveState(h serve.Health) string {
	proxy := "no run proxies"
	if h.DockerProxy {
		proxy = "per-run Docker proxies"
	}
	image := "runner image not loaded"
	if h.Image != nil {
		image = fmt.Sprintf("runner image %s (%s)", h.Image.Tag, h.Image.ID)
	}
	tools := "tool cache not prepared"
	if h.ToolCache != "" {
		tools = "tool cache " + h.ToolCache
	}
	budget := "no cache budget pass"
	if b := h.CacheBudget; b != nil {
		budget = fmt.Sprintf("cache budget %s at %s over %d namespaces", b.Status, b.At.Format(time.RFC3339), b.Namespaces)
	}
	return strings.Join([]string{proxy, image, tools, budget}, "; ")
}
