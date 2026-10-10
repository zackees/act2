package doctor

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/nektos/act/pkg/serve"
	"github.com/stretchr/testify/assert"
)

func healthy() Probes {
	files := map[string]string{
		"/sys/fs/cgroup/cgroup.controllers":     "cpuset cpu io memory pids",
		"/sys/fs/cgroup/cgroup.subtree_control": "cpu memory pids",
	}
	return Probes{
		Ping: func(context.Context) error { return nil },
		Info: func(context.Context) (DockerInfo, error) {
			return DockerInfo{CgroupVersion: "2", MemTotal: 16 << 30, DockerRootDir: "/var/lib/docker"}, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if v, ok := files[path]; ok {
				return []byte(v), nil
			}
			return nil, fs.ErrNotExist
		},
		FreeBytes: func(string) (uint64, error) { return 100 << 30, nil },
		Serve:     func(context.Context) (serve.Health, error) { return serve.Health{}, fs.ErrNotExist },
	}
}

var opts = Options{MinDiskBytes: 20 << 30, MinMemoryBytes: 4 << 30}

func byID(r Report) map[string]Check {
	out := map[string]Check{}
	for _, c := range r.Checks {
		out[c.ID] = c
	}
	return out
}

func TestHealthyHostPassesWithStableIDs(t *testing.T) {
	r := Run(context.Background(), healthy(), opts)
	assert.True(t, r.OK)
	assert.Equal(t, 1, r.SchemaVersion)
	ids := []string{}
	for _, c := range r.Checks {
		ids = append(ids, c.ID)
	}
	assert.Equal(t, []string{"docker.reachable", "docker.cgroup_v2", "docker.privileged", "memory.headroom", "disk.headroom",
		"cgroup.controllers", "cgroup.delegation", "serve.running"}, ids)
	assert.Equal(t, Skip, byID(r)["serve.running"].Status)
}

func TestOnlyRequiredFailuresFailTheReport(t *testing.T) {
	p := healthy()
	p.FreeBytes = func(string) (uint64, error) { return 1 << 30, nil }
	p.Info = func(context.Context) (DockerInfo, error) {
		return DockerInfo{CgroupVersion: "2", MemTotal: 1 << 30, DockerRootDir: "/d"}, nil
	}
	r := Run(context.Background(), p, opts)
	assert.True(t, r.OK, "headroom warnings are optional")
	assert.Equal(t, Warn, byID(r)["disk.headroom"].Status)
	assert.Equal(t, Warn, byID(r)["memory.headroom"].Status)

	p = healthy()
	p.Info = func(context.Context) (DockerInfo, error) { return DockerInfo{CgroupVersion: "1", Rootless: true}, nil }
	r = Run(context.Background(), p, opts)
	assert.False(t, r.OK)
	assert.Equal(t, Fail, byID(r)["docker.cgroup_v2"].Status)
	assert.Equal(t, Fail, byID(r)["docker.privileged"].Status)
}

func TestUnreachableDockerSkipsItsChecks(t *testing.T) {
	p := healthy()
	p.Ping = func(context.Context) error { return errors.New("no daemon") }
	r := Run(context.Background(), p, opts)
	assert.False(t, r.OK)
	c := byID(r)
	assert.Equal(t, Fail, c["docker.reachable"].Status)
	assert.NotEmpty(t, c["docker.reachable"].Remediation)
	assert.Equal(t, Skip, c["docker.cgroup_v2"].Status)
	assert.Equal(t, Pass, c["cgroup.controllers"].Status)
}

func TestCgroupControllersAndDelegation(t *testing.T) {
	p := healthy()
	p.ReadFile = func(path string) ([]byte, error) {
		if path == "/sys/fs/cgroup/cgroup.controllers" {
			return []byte("cpu io"), nil
		}
		return []byte(""), nil
	}
	r := Run(context.Background(), p, opts)
	assert.False(t, r.OK)
	assert.Contains(t, byID(r)["cgroup.controllers"].Summary, "memory pids")
	assert.Equal(t, Warn, byID(r)["cgroup.delegation"].Status)

	p.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	r = Run(context.Background(), p, opts)
	assert.Equal(t, Fail, byID(r)["cgroup.controllers"].Status)
	assert.Equal(t, Skip, byID(r)["cgroup.delegation"].Status)
}

func TestRunningServeIsReported(t *testing.T) {
	p := healthy()
	p.Serve = func(context.Context) (serve.Health, error) {
		return serve.Health{Protocol: 1, Version: "v", MaxRuns: 4, Runs: []string{"a"}}, nil
	}
	c := byID(Run(context.Background(), p, opts))["serve.running"]
	assert.Equal(t, Pass, c.Status)
	assert.Contains(t, c.Summary, "1 of 4 runs")
}
