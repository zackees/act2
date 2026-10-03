package container

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestImageExistsLocallyPlatformAPI(t *testing.T) {
	for _, tc := range []struct {
		name, api, requested, response string
		status                         int
		want, wantError, wantQuery     bool
	}{
		{"modern-arm", "1.49", "linux/arm64", `{"Os":"linux","Architecture":"arm64"}`, 200, true, false, true},
		{"modern-missing", "1.49", "linux/arm64", `{"message":"missing platform"}`, 404, false, false, true},
		{"modern-error", "1.49", "linux/arm64", `{"message":"storage failed"}`, 500, false, true, true},
		{"modern-ignored-query", "1.49", "linux/arm64", `{"Os":"linux","Architecture":"amd64"}`, 200, false, false, true},
		{"modern-variant", "1.49", "linux/arm/v7", `{"Os":"linux","Architecture":"arm","Variant":"v7"}`, 200, true, false, true},
		{"old-arm", "1.48", "linux/arm64", "", 200, false, false, false},
		{"modern-native", "1.49", "linux/amd64", "", 200, true, false, false},
		{"any", "1.49", "any", "", 200, true, false, false},
		{"unspecified", "1.49", "", "", 200, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queried := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", tc.api)
					return
				}
				assert.True(t, strings.HasSuffix(r.URL.Path, "/images/test/json"))
				if query := r.URL.Query().Get("platform"); query != "" {
					queried = true
					assert.Contains(t, query, `"os":"linux"`)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.response)
					return
				}
				_, _ = io.WriteString(w, `{"Os":"linux","Architecture":"amd64"}`)
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", server.URL)
			t.Setenv("DOCKER_API_VERSION", "")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			exists, err := ImageExistsLocally(context.Background(), "test", tc.requested)
			assert.Equal(t, tc.want, exists)
			assert.Equal(t, tc.wantError, err != nil)
			assert.Equal(t, tc.wantQuery, queried)
		})
	}
}

func TestImageExistsLocally(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	// to help make this test reliable and not flaky, we need to have
	// an image that will exist, and onew that won't exist

	// Test if image exists with specific tag
	invalidImageTag, err := ImageExistsLocally(ctx, "library/alpine:this-random-tag-will-never-exist", "linux/amd64")
	assert.Nil(t, err)
	assert.Equal(t, false, invalidImageTag)

	// Test if image exists with specific architecture (image platform)
	invalidImagePlatform, err := ImageExistsLocally(ctx, "alpine:latest", "windows/amd64")
	assert.Nil(t, err)
	assert.Equal(t, false, invalidImagePlatform)

	// pull an image
	cli, err := client.New(client.FromEnv)
	assert.Nil(t, err)

	// Chose alpine latest because it's so small
	// maybe we should build an image instead so that tests aren't reliable on dockerhub
	readerDefault, err := cli.ImagePull(ctx, "node:16-buster-slim", client.ImagePullOptions{
		Platforms: []specs.Platform{{OS: "linux", Architecture: "amd64"}},
	})
	assert.Nil(t, err)
	defer readerDefault.Close()
	_, err = io.ReadAll(readerDefault)
	assert.Nil(t, err)

	imageDefaultArchExists, err := ImageExistsLocally(ctx, "node:16-buster-slim", "linux/amd64")
	assert.Nil(t, err)
	assert.Equal(t, true, imageDefaultArchExists)

	// Validate if another architecture platform can be pulled
	readerArm64, err := cli.ImagePull(ctx, "node:16-buster-slim", client.ImagePullOptions{
		Platforms: []specs.Platform{{OS: "linux", Architecture: "arm64"}},
	})
	assert.Nil(t, err)
	defer readerArm64.Close()
	_, err = io.ReadAll(readerArm64)
	assert.Nil(t, err)

	imageArm64Exists, err := ImageExistsLocally(ctx, "node:16-buster-slim", "linux/arm64")
	assert.Nil(t, err)
	assert.Equal(t, true, imageArm64Exists)
}
