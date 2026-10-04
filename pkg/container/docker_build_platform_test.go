//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

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
	for _, platform := range []string{"linux/arm64", ""} {
		name := "act-build-platform-native"
		expected := "amd64"
		if platform != "" {
			name = "act-build-platform-arm"
			expected = "arm64"
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
