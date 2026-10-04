package artifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/julienschmidt/httprouter"
)

func TestV4BlocksCommitInDeclaredOrder(t *testing.T) {
	router := httprouter.New()
	base := t.TempDir()
	fsys := readWriteFSImpl{}
	RoutesV4(router, base, fsys, fsys)

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	endpoint := "http://localhost" + ArtifactV4RouteBase
	created := request(http.MethodPost, endpoint+"/CreateArtifact",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle","version":4}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", created.Code, created.Body.String())
	}
	var upload struct {
		SignedUploadURL string `json:"signedUploadUrl"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	blockURL := func(comp, id string) string {
		t.Helper()
		parsed, err := url.Parse(upload.SignedUploadURL)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		query.Set("comp", comp)
		if id != "" {
			query.Set("blockid", id)
		}
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	// Azure uploads several blocks concurrently. Deliberately deliver the
	// second chunk first: append order must not become archive byte order.
	for _, chunk := range []struct{ id, data string }{{"second", "B"}, {"first", "A"}} {
		response := request(http.MethodPut, blockURL("block", chunk.id), chunk.data)
		if response.Code != http.StatusCreated {
			t.Fatalf("block %s status %d", chunk.id, response.Code)
		}
	}
	missing := request(http.MethodPut, blockURL("blocklist", ""),
		"<BlockList><Latest>missing</Latest></BlockList>")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing block status %d; want 400", missing.Code)
	}
	committed := request(http.MethodPut, blockURL("blocklist", ""),
		"<BlockList><Latest>first</Latest><Latest>second</Latest></BlockList>")
	if committed.Code != http.StatusCreated {
		t.Fatalf("block list status %d: %s", committed.Code, committed.Body.String())
	}
	wrongSize := request(http.MethodPost, endpoint+"/FinalizeArtifact",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle","size":"3"}`)
	if wrongSize.Code != http.StatusBadRequest {
		t.Fatalf("wrong size status %d; want 400", wrongSize.Code)
	}
	wrongHash := request(http.MethodPost, endpoint+"/FinalizeArtifact",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle","size":"2","hash":"sha256:deadbeef"}`)
	if wrongHash.Code != http.StatusBadRequest {
		t.Fatalf("wrong hash status %d; want 400", wrongHash.Code)
	}
	hash := sha256.Sum256([]byte("AB"))
	finalized := request(http.MethodPost, endpoint+"/FinalizeArtifact",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle","size":"2","hash":"sha256:`+fmt.Sprintf("%x", hash)+`"}`)
	if finalized.Code != http.StatusOK {
		t.Fatalf("finalize status %d: %s", finalized.Code, finalized.Body.String())
	}
	signed := request(http.MethodPost, endpoint+"/GetSignedArtifactURL",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","name":"bundle"}`)
	var download struct {
		SignedURL string `json:"signedUrl"`
	}
	if err := json.Unmarshal(signed.Body.Bytes(), &download); err != nil {
		t.Fatal(err)
	}
	got := request(http.MethodGet, download.SignedURL, "")
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), []byte("AB")) {
		t.Fatalf("download status %d, body %q; want AB", got.Code, got.Body.String())
	}
	listed := request(http.MethodPost, endpoint+"/ListArtifacts",
		`{"workflowRunBackendId":"1","workflowJobRunBackendId":"2","nameFilter":"bundle"}`)
	var listing struct {
		Artifacts []struct {
			Size int64 `json:"size,string"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Artifacts) != 1 || listing.Artifacts[0].Size != 2 {
		t.Fatalf("listed artifact: %s", listed.Body.String())
	}
}
