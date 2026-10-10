//go:build linux

package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/moby/moby/client"
)

// CgroupRoot is the cgroup v2 mount the Linux isolator manages.
const CgroupRoot = "/sys/fs/cgroup"

// linuxIsolator scopes runs with cgroup v2 and the engine's Docker.
type linuxIsolator struct {
	cfg    Config
	docker client.APIClient
}

// NewIsolator returns the cgroup v2 and Docker isolator for cfg.
func NewIsolator(cfg Config) (Isolator, error) {
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &linuxIsolator{cfg: cfg, docker: docker}, nil
}

func (l *linuxIsolator) cgroup(scope Scope) string { return filepath.Join(CgroupRoot, scope.Cgroup) }

func writeFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil { //nolint:gosec // cgroup files need group/other read
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (l *linuxIsolator) Open(ctx context.Context, scope Scope) error {
	cg := l.cgroup(scope)
	if err := os.MkdirAll(filepath.Join(cg, "act"), 0o755); err != nil { //nolint:gosec // scope names derive from a validated run key
		return err
	}
	limits := scope.Limits
	for _, set := range [][2]string{
		{"cgroup.subtree_control", "+cpu +memory +pids"},
		{"memory.max", strconv.FormatUint(limits.MemoryBytes, 10)},
		{"cpu.max", limits.CPUMax()},
		{"pids.max", strconv.FormatUint(limits.Pids, 10)},
	} {
		if err := writeFile(filepath.Join(cg, set[0]), set[1]); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(cg, "memory.swap.max")); err == nil { //nolint:gosec // scope names derive from a validated run key
		if err := writeFile(filepath.Join(cg, "memory.swap.max"), "0"); err != nil {
			return err
		}
	}
	networks, err := l.docker.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("name", scope.Network)})
	if err != nil {
		return err
	}
	for _, network := range networks.Items {
		if network.Name == scope.Network {
			return nil
		}
	}
	_, err = l.docker.NetworkCreate(ctx, scope.Network, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{l.cfg.RunLabel: scope.RunID},
	})
	return err
}

func (l *linuxIsolator) Prepare(cmd *exec.Cmd, scope Scope) (func(), error) {
	dir, err := os.Open(filepath.Join(l.cgroup(scope), "act"))
	if err != nil {
		return func() {}, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd()), Setpgid: true}
	return func() { dir.Close() }, nil
}

func (l *linuxIsolator) Kill(scope Scope) error {
	path := filepath.Join(l.cgroup(scope), "act", "cgroup.kill")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return writeFile(path, "1")
}

// labelled lists the run's containers (labelled, or attached to its
// network: a container created without the label is still the run's),
// networks and volumes.
func (l *linuxIsolator) labelled(ctx context.Context, runID, network string) (containers, networks, volumes []string, err error) {
	filter := make(client.Filters).Add("label", l.cfg.RunLabel+"="+runID)
	if runID == "" {
		// Only the network's containers, and the network itself.
		filter = make(client.Filters).Add("name", "^"+network+"$")
	}
	seen := map[string]bool{}
	var cs client.ContainerListResult
	if runID != "" {
		if cs, err = l.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter}); err != nil {
			return nil, nil, nil, err
		}
	}
	if network != "" {
		// A missing network has no containers; the filter errors on it.
		if attached, err := l.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("network", network)}); err == nil {
			cs.Items = append(cs.Items, attached.Items...)
		}
	}
	for _, c := range cs.Items {
		if !seen[c.ID] {
			seen[c.ID] = true
			containers = append(containers, c.ID)
		}
	}
	ns, err := l.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, n := range ns.Items {
		networks = append(networks, n.ID)
	}
	if runID == "" {
		return containers, networks, nil, nil
	}
	vs, err := l.docker.VolumeList(ctx, client.VolumeListOptions{Filters: filter})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, v := range vs.Items {
		volumes = append(volumes, v.Name)
	}
	return containers, networks, volumes, nil
}

func (l *linuxIsolator) Close(ctx context.Context, scope Scope) error {
	if err := l.Kill(scope); err != nil {
		return err
	}
	if err := l.removeLabelled(ctx, scope.RunID, scope.Network); err != nil {
		return err
	}
	if err := os.RemoveAll(scope.Work); err != nil {
		return err
	}
	return removeCgroup(l.cgroup(scope))
}

func (l *linuxIsolator) removeLabelled(ctx context.Context, runID, network string) error {
	containers, networks, volumes, err := l.labelled(ctx, runID, network)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range containers {
		_, err := l.docker.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		errs = append(errs, err)
	}
	for _, id := range networks {
		_, err := l.docker.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
		errs = append(errs, err)
	}
	for _, name := range volumes {
		_, err := l.docker.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
		errs = append(errs, err)
	}
	containers, networks, volumes, err = l.labelled(ctx, runID, network)
	if err != nil {
		return err
	}
	if left := len(containers) + len(networks) + len(volumes); left > 0 {
		errs = append(errs, fmt.Errorf("run %s: %d containers, %d networks and %d volumes remain after cleanup",
			runID, len(containers), len(networks), len(volumes)))
		return errors.Join(errs...)
	}
	return nil
}

// removeCgroup removes a cgroup tree once its processes have exited.
func removeCgroup(cg string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var dirs []string
		_ = filepath.WalkDir(cg, func(path string, entry os.DirEntry, err error) error { //nolint:gosec // scope names derive from a validated run key
			if err == nil && entry.IsDir() {
				dirs = append(dirs, path)
			}
			return nil
		})
		for i := len(dirs) - 1; i >= 0; i-- {
			_ = syscall.Rmdir(dirs[i])
		}
		if _, err := os.Stat(cg); errors.Is(err, os.ErrNotExist) { //nolint:gosec // scope names derive from a validated run key
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run cgroup %s is still busy", cg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (l *linuxIsolator) Reap(ctx context.Context) error {
	// Every labelled object in the engine is a previous server's: no run is
	// live before this server admits one.
	filter := make(client.Filters).Add("label", l.cfg.RunLabel)
	runs := map[string]bool{}
	cs, err := l.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return err
	}
	for _, c := range cs.Items {
		runs[c.Labels[l.cfg.RunLabel]] = true
	}
	ns, err := l.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return err
	}
	for _, n := range ns.Items {
		runs[n.Labels[l.cfg.RunLabel]] = true
	}
	vs, err := l.docker.VolumeList(ctx, client.VolumeListOptions{Filters: filter})
	if err != nil {
		return err
	}
	for _, v := range vs.Items {
		runs[v.Labels[l.cfg.RunLabel]] = true
	}
	var errs []error
	var cgroups []string
	entries, _ := os.ReadDir(CgroupRoot)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), l.cfg.ScopePrefix) {
			cg := filepath.Join(CgroupRoot, entry.Name())
			cgroups = append(cgroups, cg)
			if _, err := os.Stat(filepath.Join(cg, "act", "cgroup.kill")); err == nil {
				errs = append(errs, writeFile(filepath.Join(cg, "act", "cgroup.kill"), "1"))
			}
			// Its network's unlabelled containers are the run's too.
			errs = append(errs, l.removeLabelled(ctx, "", entry.Name()))
		}
	}
	for runID := range runs {
		errs = append(errs, l.removeLabelled(ctx, runID, ""))
	}
	for _, cg := range cgroups {
		errs = append(errs, removeCgroup(cg))
	}
	errs = append(errs, os.RemoveAll(l.cfg.WorkRoot))
	return errors.Join(errs...)
}
