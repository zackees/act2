//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/moby/moby/api/types/system"

	"github.com/moby/moby/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// An ARM-only cached base must not break a later ordinary native Docker action.
// The stock setup-qemu action supplies emulation in the isolated workflow.
func TestDockerBuildAfterForeignPlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	cli, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	require.NoError(t, err)
	native := nativeDockerBuildPlatform(info.Info).Architecture
	foreign := "arm64"
	if native == foreign {
		foreign = "amd64"
	}
	for _, platform := range []string{"linux/" + foreign, ""} {
		name := "act-build-platform-native"
		expected := native
		if platform != "" {
			name = "act-build-platform-arm"
			expected = foreign
		}
		var body bytes.Buffer
		writer := tar.NewWriter(&body)
		dockerfile := []byte("FROM ubuntu:18.04\n")
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(dockerfile))}))
		_, err = writer.Write(dockerfile)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		err = NewDockerBuildExecutor(NewDockerBuildExecutorInput{BuildContext: &body, Dockerfile: "Dockerfile", ImageTag: name, Platform: platform})(ctx)
		require.NoError(t, err, "build requested platform %q after foreign-platform base", platform)
		image, err := cli.ImageInspect(ctx, name, client.ImageInspectWithPlatform(&specs.Platform{OS: "linux", Architecture: expected}))
		require.NoError(t, err)
		require.Equal(t, expected, image.Architecture, "builder must not silently reuse foreign-platform image")
	}
}

func TestNativeDockerBuildPlatformAliases(t *testing.T) {
	for _, test := range []struct{ reported, architecture, variant string }{
		{"x86_64", "amd64", ""}, {"amd64", "amd64", ""},
		{"aarch64", "arm64", ""}, {"arm64", "arm64", ""},
		{"i386", "386", ""}, {"i686", "386", ""},
		{"armv6l", "arm", "v6"}, {"armv7l", "arm", "v7"},
		{"ppc64le", "ppc64le", ""}, {"s390x", "s390x", ""},
	} {
		t.Run(test.reported, func(t *testing.T) {
			got := nativeDockerBuildPlatform(system.Info{OSType: "linux", Architecture: test.reported})
			require.Equal(t, specs.Platform{OS: "linux", Architecture: test.architecture, Variant: test.variant}, got)
		})
	}
}
