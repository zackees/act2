package artifacts

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Azure block uploads may finish in a different order from their committed list.
// Exercise the stock v4 action's block protocol with a real ZIP and three readers.
func TestV4BlockUploadConcurrentDownloads(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("bundle/payload.txt")
	require.NoError(t, err)
	payload := strings.Repeat("bundle integrity\n", 4096)
	_, err = io.WriteString(entry, payload)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	data := archive.Bytes()
	router := httprouter.New()
	fsys := readWriteFSImpl{}
	base := t.TempDir()
	RoutesV4(router, base, fsys, fsys)
	server := httptest.NewServer(router)
	defer server.Close()
	route := artifactV4Routes{AppURL: strings.TrimPrefix(server.URL, "http://"), prefix: ArtifactV4RouteBase}
	request := func(method, target string, body io.Reader) bool {
		req, requestErr := http.NewRequest(method, target, body)
		if !assert.NoError(t, requestErr) {
			return false
		}
		response, requestErr := server.Client().Do(req)
		if !assert.NoError(t, requestErr) {
			return false
		}
		defer response.Body.Close()
		_, requestErr = io.Copy(io.Discard, response.Body)
		return assert.NoError(t, requestErr) && assert.Less(t, response.StatusCode, 300)
	}
	create := `{"workflow_run_backend_id":"1","workflow_job_run_backend_id":"1","name":"bundle","version":4}`
	if !request(http.MethodPost, server.URL+ArtifactV4RouteBase+"/CreateArtifact", strings.NewReader(create)) {
		return
	}
	upload := route.buildArtifactURL("UploadArtifact", "bundle", 1)
	middle := len(data) / 2
	// Both valid blocks arrive successfully, in reverse logical order.
	if !request(http.MethodPut, upload+"&comp=block&blockid=YmxvY2sy", bytes.NewReader(data[middle:])) {
		return
	}
	if !request(http.MethodPut, upload+"&comp=block&blockid=YmxvY2sx", bytes.NewReader(data[:middle])) {
		return
	}
	// A successful retry replaces the same block instead of appending duplicate bytes.
	if !request(http.MethodPut, upload+"&comp=block&blockid=YmxvY2sx", bytes.NewReader(data[:middle])) {
		return
	}
	if !request(http.MethodPut, upload+"&comp=blocklist", strings.NewReader(`<BlockList><Latest>YmxvY2sx</Latest><Latest>YmxvY2sy</Latest></BlockList>`)) {
		return
	}
	// Rejected block lists must leave the previously committed ZIP intact.
	for _, invalid := range []string{
		`<BlockList><Latest>missing-block</Latest></BlockList>`,
		`<BlockList><Unknown>YmxvY2sx</Unknown></BlockList>`,
		`<BlockList><Latest></BlockList>`,
	} {
		req, requestErr := http.NewRequest(http.MethodPut, upload+"&comp=blocklist", strings.NewReader(invalid))
		if !assert.NoError(t, requestErr) {
			return
		}
		response, requestErr := server.Client().Do(req)
		if !assert.NoError(t, requestErr) {
			return
		}
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
		_ = response.Body.Close()
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, requestErr := server.Client().Get(route.buildArtifactURL("DownloadArtifact", "bundle", 1))
			if !assert.NoError(t, requestErr) {
				return
			}
			defer response.Body.Close()
			got, requestErr := io.ReadAll(response.Body)
			if !assert.NoError(t, requestErr) {
				return
			}
			if !assert.True(t, bytes.Equal(data, got), "committed ZIP differs from upload") {
				return
			}
			zr, requestErr := zip.NewReader(bytes.NewReader(got), int64(len(got)))
			if !assert.NoError(t, requestErr) {
				return
			}
			if !assert.Len(t, zr.File, 1) {
				return
			}
			reader, requestErr := zr.File[0].Open()
			if !assert.NoError(t, requestErr) {
				return
			}
			defer reader.Close()
			content, requestErr := io.ReadAll(reader)
			assert.NoError(t, requestErr)
			assert.Equal(t, payload, string(content), "extracted payload differs")
		}()
	}
	wg.Wait()
}
