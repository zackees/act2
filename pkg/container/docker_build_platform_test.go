//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/system"

	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
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
		image, err := inspectBuiltPlatform(ctx, cli, name, expected)
		require.NoError(t, err)
		require.Equal(t, "linux", image.Os)
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

func inspectBuiltPlatform(ctx context.Context, cli client.APIClient, name, expected string) (client.ImageInspectResult, error) {
	image, err := cli.ImageInspect(ctx, name)
	if err != nil || versions.LessThan(cli.ClientVersion(), "1.49") {
		return image, err
	}
	return cli.ImageInspect(ctx, name, client.ImageInspectWithPlatform(&specs.Platform{OS: "linux", Architecture: expected}))
}

func TestBuiltPlatformInspectionAPICompatibility(t *testing.T) {
	for _, api := range []string{"1.48", "1.49"} {
		t.Run(api, func(t *testing.T) {
			platformQueries := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", api)
					return
				}
				require.True(t, strings.HasSuffix(r.URL.Path, "/images/built/json"))
				if r.URL.Query().Get("platform") != "" {
					platformQueries++
				}
				_, err := w.Write([]byte(`{"Os":"linux","Architecture":"arm64"}`))
				require.NoError(t, err)
			}))
			defer server.Close()
			cli, err := client.New(client.WithHost(server.URL))
			require.NoError(t, err)
			defer cli.Close()
			image, err := inspectBuiltPlatform(context.Background(), cli, "built", "arm64")
			require.NoError(t, err)
			require.Equal(t, "linux", image.Os)
			require.Equal(t, "arm64", image.Architecture)
			expectedQueries := 0
			if api == "1.49" {
				expectedQueries = 1
			}
			require.Equal(t, expectedQueries, platformQueries)
		})
	}
}
