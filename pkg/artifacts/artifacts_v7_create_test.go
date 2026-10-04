package artifacts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/julienschmidt/httprouter"
)

// The pinned upload-artifact v7.0.1 request adds protobuf StringValue field 6.
// Wrapper values are JSON strings, rather than objects containing "value".
// These fixtures run only in the isolated runner; its scratch files are retained.
func TestV7CreateArtifactRequest(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		body     string
		accepted bool
	}{
		{"legacy", `{"workflow_run_backend_id":"1","workflow_job_run_backend_id":"2","name":"bundle","version":4}`, true},
		{"v7-snake", `{"workflow_run_backend_id":"1","workflow_job_run_backend_id":"2","name":"bundle","version":7,"mime_type":"application/zip"}`, true},
		{"v7-camel", `{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle","version":7,"mimeType":"application/zip"}`, true},
		{"unknown-field", `{"workflow_run_backend_id":"1","name":"bundle","version":7,"mime_type":"application/zip","unexpected":true}`, false},
		{"wrong-type", `{"workflow_run_backend_id":"1","name":"bundle","version":7,"mime_type":42}`, false},
		{"wrapper-object", `{"workflow_run_backend_id":"1","name":"bundle","version":7,"mime_type":{"value":"application/zip"}}`, false},
		{"malformed", `{"workflow_run_backend_id":"1","name":"bundle","version":7,"mime_type":"application/zip"`, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			base, err := os.MkdirTemp("", "act2-artifact-v7-")
			if err != nil {
				t.Fatal(err)
			}
			router := httprouter.New()
			fsys := readWriteFSImpl{}
			RoutesV4(router, base, fsys, fsys)
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Post(server.URL+ArtifactV4RouteBase+"/CreateArtifact", "application/json", strings.NewReader(fixture.body))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if fixture.accepted {
				if response.StatusCode != http.StatusOK {
					t.Fatalf("CreateArtifact status=%d; want 200", response.StatusCode)
				}
				var created struct {
					OK  bool   `json:"ok"`
					URL string `json:"signedUploadUrl"`
				}
				if err := json.Unmarshal(body, &created); err != nil {
					t.Fatal(err)
				}
				if !created.OK || created.URL == "" {
					t.Fatal("CreateArtifact omitted success or upload URL")
				}
			} else {
				if response.StatusCode < 400 {
					t.Fatalf("invalid request accepted: status=%d", response.StatusCode)
				}
				entries, err := os.ReadDir(base)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatal("invalid request created artifact storage")
				}
			}
		})
	}
}
