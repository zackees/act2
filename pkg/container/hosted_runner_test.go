//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedRunnerIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker hosted-runner integration")
	}
	for _, hosted := range []bool{true, false} {
		t.Run(fmt.Sprint(hosted), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var output bytes.Buffer
			cr := NewContainer(&NewContainerInput{
				Image:        "docker.io/catthehacker/ubuntu@sha256:4f2d5083a9d10d018c1c511eb8665cd480553c11975e78fd903a46daa830768b",
				Name:         fmt.Sprintf("act2-hosted-user-test-%d", time.Now().UnixNano()),
				Entrypoint:   []string{"tail", "-f", "/dev/null"},
				WorkingDir:   "/tmp/workspace",
				HostedRunner: hosted,
				Stdout:       &output, Stderr: &output,
			})
			require.NoError(t, cr.Create(nil, nil)(ctx))
			defer func() { assert.NoError(t, cr.Close()(context.Background())) }()
			defer func() { assert.NoError(t, cr.Remove()(context.Background())) }()
			require.NoError(t, cr.Start(false)(ctx))
			env := map[string]string{}
			require.NoError(t, cr.UpdateFromImageEnv(&env)(ctx))
			if hosted {
				require.NoError(t, cr.Exec([]string{"bash", "-ec", `test "$(id -u)" -ne 0; test "$HOME" = /home/actrunner; touch "$HOME/writable" /tmp/workspace/writable /opt/hostedtoolcache/writable; d=$(mktemp -d); chmod 500 "$d"; if touch "$d/denied"; then exit 1; fi; sudo -n true; node -e 'if(process.getuid()===0)process.exit(1)'`}, env, "", "")(ctx), output.String())
				require.NoError(t, cr.Exec([]string{"id", "-u"}, nil, "0", "")(ctx))
				assert.Contains(t, output.String(), "Permission denied")
			} else {
				require.NoError(t, cr.Exec([]string{"bash", "-ec", `test "$(id -u)" -eq 0; d=$(mktemp -d); chmod 500 "$d"; touch "$d/allowed"`}, env, "", "")(ctx), output.String())
			}
		})
	}
}

func TestHostedRunnerDoesNotChownBinds(t *testing.T) {
	cr := &containerReference{input: &NewContainerInput{Binds: []string{"/host/source:/workspace:rw", "/host/cache:/opt/hostedtoolcache/seed:ro"}}}
	assert.False(t, cr.mayChown("/workspace"))
	assert.False(t, cr.mayChown("/workspace/subdir"))
	assert.False(t, cr.mayChown("/opt/hostedtoolcache"))
	assert.True(t, cr.mayChown("/var/run/act"))
}

// Root-owned completed caches remain usable, and a bound checkout's existing
// ownership survives the migration to the ordinary runner identity.
func TestHostedRunnerSeedAndBoundOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker hosted-runner integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := fmt.Sprintf("act2-bound-user-test-%d", time.Now().UnixNano())
	cli, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer cli.Close()
	defer func() {
		_, err := cli.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true})
		assert.NoError(t, err)
	}()
	var output bytes.Buffer
	cr := NewContainer(&NewContainerInput{
		Image: "docker.io/catthehacker/ubuntu@sha256:4f2d5083a9d10d018c1c511eb8665cd480553c11975e78fd903a46daa830768b",
		Name:  name, Entrypoint: []string{"tail", "-f", "/dev/null"}, WorkingDir: "/workspace",
		Binds: []string{name + ":/workspace:rw"}, Stdout: &output, Stderr: &output,
	}).(*containerReference)
	require.NoError(t, cr.Create(nil, nil)(ctx))
	defer func() { assert.NoError(t, cr.Close()(context.Background())) }()
	defer func() { assert.NoError(t, cr.Remove()(context.Background())) }()
	require.NoError(t, cr.Start(false)(ctx))
	require.NoError(t, cr.Exec([]string{"sh", "-ec", `echo retained > /workspace/keep; chown -R 1000:1000 /workspace; mkdir -p /opt/hostedtoolcache/seed; echo complete > /opt/hostedtoolcache/seed/marker; chmod 755 /opt/hostedtoolcache/seed; chmod 644 /opt/hostedtoolcache/seed/marker`}, nil, "0", "")(ctx))
	cr.input.HostedRunner = true
	require.NoError(t, cr.provisionHostedRunner()(ctx), output.String())
	env := map[string]string{}
	require.NoError(t, cr.UpdateFromImageEnv(&env)(ctx))
	require.NoError(t, cr.Exec([]string{"bash", "-ec", `test "$(id -u)" -eq 1000; test "$(stat -c '%u:%g' /workspace/keep)" = 1000:1000; test "$(cat /workspace/keep)" = retained; touch /workspace/new; test "$(cat /opt/hostedtoolcache/seed/marker)" = complete; touch /opt/hostedtoolcache/seed/new; sudo -n true`}, env, "", "")(ctx), output.String())
}

func TestHostedRunnerProtectsOptionMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker hosted-runner integration")
	}
	ctx := context.Background()
	cli, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer cli.Close()
	name := fmt.Sprintf("act2-protected-mount-test-%d", time.Now().UnixNano())
	fixture, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name})
	require.NoError(t, err)
	defer func() {
		_, err := cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
		assert.NoError(t, err)
	}()
	for _, target := range []string{"/home", "/home/actrunner", "/opt/hostedtoolcache", "/var/run/act", "/etc/sudoers.d", "/var/mail"} {
		t.Run(target, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var output bytes.Buffer
			cr := NewContainer(&NewContainerInput{
				Image:      "docker.io/catthehacker/ubuntu@sha256:4f2d5083a9d10d018c1c511eb8665cd480553c11975e78fd903a46daa830768b",
				Name:       fmt.Sprintf("act2-option-user-test-%d", time.Now().UnixNano()),
				Entrypoint: []string{"tail", "-f", "/dev/null"}, WorkingDir: "/tmp/workspace", HostedRunner: true,
				// Read-only mount prevents any host mutation even if the regression returns.
				NetworkMode: "bridge",
				Options:     "--mount type=bind,source=" + fixture.Volume.Mountpoint + ",target=" + target + ",readonly",
				Stdout:      &output, Stderr: &output,
			}).(*containerReference)
			require.NoError(t, cr.Create(nil, nil)(ctx))
			defer func() { assert.NoError(t, cr.Close()(context.Background())) }()
			defer func() { assert.NoError(t, cr.Remove()(context.Background())) }()
			err := cr.Start(false)(ctx)
			if target == "/home" || target == hostedRunnerHome {
				require.ErrorContains(t, err, "cannot provision bind-mounted HOME")
			} else if target == "/etc/sudoers.d" || target == "/var/mail" {
				require.ErrorContains(t, err, "cannot provision host-bound account configuration")
			} else {
				require.NoError(t, err, output.String())
				assert.False(t, cr.mayChown(target))
			}
		})
	}
}

// A hosted runner owns its private workspace parent and can move generated
// scripts, including a custom shell that turns the script into a Dockerfile.
func TestHostedRunnerWorkspaceLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker hosted-runner integration")
	}
	cases := []struct {
		name, workdir, command string
		generated              bool
	}{
		{name: "private-parent", workdir: "/home/actrunner/work/repo/repo", command: `touch "$(dirname "$PWD")/sibling"`},
		{name: "generated-script", workdir: "/tmp/workspace", generated: true, command: `mv /var/run/act/workflow/movable ./Dockerfile`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var output bytes.Buffer
			cr := NewContainer(&NewContainerInput{
				Image:      "docker.io/catthehacker/ubuntu@sha256:4f2d5083a9d10d018c1c511eb8665cd480553c11975e78fd903a46daa830768b",
				Name:       fmt.Sprintf("act2-workspace-user-test-%d", time.Now().UnixNano()),
				Entrypoint: []string{"tail", "-f", "/dev/null"}, WorkingDir: test.workdir, HostedRunner: true,
				Stdout: &output, Stderr: &output,
			})
			require.NoError(t, cr.Create(nil, nil)(ctx))
			defer func() { assert.NoError(t, cr.Close()(context.Background())) }()
			defer func() { assert.NoError(t, cr.Remove()(context.Background())) }()
			require.NoError(t, cr.Start(false)(ctx), output.String())
			if test.generated {
				require.NoError(t, cr.Copy("/var/run/act", &FileEntry{Name: "workflow/movable", Mode: 0755, Body: "FROM ubuntu:latest\n"})(ctx))
			}
			require.NoError(t, cr.Exec([]string{"bash", "-ec", test.command}, nil, "", "")(ctx), output.String())
		})
	}
}
