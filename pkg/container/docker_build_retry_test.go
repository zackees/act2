//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
 "archive/tar"
 "bytes"
 "context"
 "encoding/json"
 "io"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync"
 "testing"

 "github.com/stretchr/testify/assert"
 "github.com/stretchr/testify/require"
)

func TestDockerBuildPlatformRepair(t *testing.T) {
 for _, test := range []struct {
  name, dockerfile, initial, repaired string
  inspectStatus, retryStatus, wantBuilds int
  wantError bool
 }{
  {"initial-error", "FROM ubuntu:18.04\n", "amd64", "amd64", 200, 200, 1, true},
  {"mismatched-cache", "FROM ubuntu:18.04\n", "arm64", "amd64", 200, 200, 2, false},
  {"matching-local-offline", "FROM local-only:latest\n", "amd64", "amd64", 200, 200, 1, false},
  {"retry-still-wrong", "FROM ubuntu:18.04\n", "arm64", "arm64", 200, 200, 2, true},
  {"retry-error", "FROM ubuntu:18.04\n", "arm64", "amd64", 200, 500, 2, true},
  {"inspect-error", "FROM ubuntu:18.04\n", "arm64", "amd64", 500, 200, 1, true},
  {"explicit-final-platform", "FROM --platform=linux/arm64 ubuntu:18.04\n", "arm64", "arm64", 200, 200, 1, false},
  {"global-arg-platform", "ARG FINAL=linux/arm64\nFROM --platform=$FINAL ubuntu:18.04\n", "arm64", "arm64", 200, 200, 1, false},
  {"inherited-stage-platform", "FROM --platform=linux/arm64 ubuntu:18.04 AS prior\nFROM prior\n", "arm64", "arm64", 200, 200, 1, false},
  {"final-stage-target", "FROM --platform=linux/arm64 ubuntu:18.04 AS prior\nFROM ubuntu:18.04\nCOPY --from=prior /tmp /tmp\n", "arm64", "amd64", 200, 200, 2, false},
 } {
  t.Run(test.name, func(t *testing.T) {
   var mu sync.Mutex
   var contexts [][]byte
   pulls := []string{}
   server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    mu.Lock()
    defer mu.Unlock()
    switch {
    case r.URL.Path == "/_ping":
     w.Header().Set("API-Version", "1.48")
    case strings.HasSuffix(r.URL.Path, "/info"):
     _, _ = io.WriteString(w, `{"OSType":"linux","Architecture":"amd64"}`)
    case strings.HasSuffix(r.URL.Path, "/build"):
     body, err := io.ReadAll(r.Body)
     assert.NoError(t, err)
     contexts = append(contexts, body)
     pulls = append(pulls, r.URL.Query().Get("pull"))
     assert.Equal(t, "linux/amd64", r.URL.Query().Get("platform"))
     assert.Empty(t, r.URL.Query().Get("version"), "same classic backend")
     if test.name == "initial-error" || (len(contexts) == 2 && test.retryStatus != 200) {
      w.WriteHeader(500)
      _, _ = io.WriteString(w, `{"message":"forced pull failed"}`)
      return
     }
     _, _ = io.WriteString(w, `{ "stream":"built" }`+"\n")
    case strings.HasSuffix(r.URL.Path, "/images/test-output/json"):
     w.WriteHeader(test.inspectStatus)
     if test.inspectStatus != 200 {
      _, _ = io.WriteString(w, `{"message":"inspection failed"}`)
      return
     }
     architecture := test.initial
     if len(contexts) == 2 { architecture = test.repaired }
     _ = json.NewEncoder(w).Encode(struct { Os, Architecture string }{"linux", architecture})
    default:
     t.Errorf("unexpected request %s", r.URL.Path)
     w.WriteHeader(404)
    }
   }))
   defer server.Close()
   t.Setenv("DOCKER_HOST", server.URL)
   t.Setenv("DOCKER_API_VERSION", "")
   t.Setenv("DOCKER_TLS_VERIFY", "")
   t.Setenv("DOCKER_CERT_PATH", "")
   var body bytes.Buffer
   writer := tar.NewWriter(&body)
   require.NoError(t, writer.WriteHeader(&tar.Header{Name:"Dockerfile",Mode:0644,Size:int64(len(test.dockerfile))}))
   _, err := io.WriteString(writer, test.dockerfile)
   require.NoError(t, err)
   require.NoError(t, writer.Close())
   err = NewDockerBuildExecutor(NewDockerBuildExecutorInput{BuildContext:&body,Dockerfile:"Dockerfile",ImageTag:"test-output"})(context.Background())
   require.Equal(t, test.wantError, err != nil)
   mu.Lock()
   defer mu.Unlock()
   require.Len(t, contexts, test.wantBuilds)
   require.Empty(t, pulls[0], "ordinary cached/local builds must not force network")
   if test.wantBuilds == 2 {
    require.Equal(t, "1", pulls[1])
    require.Equal(t, contexts[0], contexts[1], "retry exact byte-identical context")
   }
  })
 }
}
