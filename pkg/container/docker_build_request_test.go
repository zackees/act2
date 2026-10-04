//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerBuildPlatformRequest(t *testing.T) {
	for _, test := range []struct {
		name, requested, info, want string
		infoStatus                  int
		wantError                   bool
	}{
		{"native", "", `{"OSType":"linux","Architecture":"x86_64"}`, "linux/amd64", 200, false},
		{"native-arm", "", `{"OSType":"linux","Architecture":"aarch64"}`, "linux/arm64", 200, false},
		{"native-windows", "", `{"OSType":"windows","Architecture":"amd64"}`, "windows/amd64", 200, false},
		{"explicit", "linux/arm64", "", "linux/arm64", 200, false},
		{"info-error", "", `{"message":"daemon info unavailable"}`, "", 500, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var infoCalls, buildCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.54")
				case strings.HasSuffix(r.URL.Path, "/info"):
					infoCalls.Add(1)
					w.WriteHeader(test.infoStatus)
					_, _ = io.WriteString(w, test.info)
				case strings.HasSuffix(r.URL.Path, "/build"):
					buildCalls.Add(1)
					assert.Equal(t, test.want, r.URL.Query().Get("platform"))
					assert.Equal(t, "Dockerfile.custom", r.URL.Query().Get("dockerfile"))
					assert.Equal(t, "test-output", r.URL.Query().Get("t"))
					assert.Empty(t, r.URL.Query().Get("pull"), "cached builds retain ordinary pull policy")
					assert.Empty(t, r.URL.Query().Get("version"), "existing builder backend is preserved")
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, `{ "stream":"built" }`+"\n")
				default:
					t.Errorf("unexpected Docker API request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", server.URL)
			t.Setenv("DOCKER_API_VERSION", "")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			err := NewDockerBuildExecutor(NewDockerBuildExecutorInput{BuildContext: strings.NewReader("context"), Dockerfile: "Dockerfile.custom", ImageTag: "test-output", Platform: test.requested})(context.Background())
			if test.wantError {
				require.ErrorContains(t, err, "daemon info unavailable")
				require.Zero(t, buildCalls.Load())
			} else {
				require.NoError(t, err)
				require.Equal(t, int64(1), buildCalls.Load())
			}
			if test.requested == "" {
				require.Equal(t, int64(1), infoCalls.Load())
			} else {
				require.Zero(t, infoCalls.Load())
			}
		})
	}
}
