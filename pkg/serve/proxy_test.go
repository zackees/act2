//go:build !windows

package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDocker answers the few Docker API calls the proxy makes or forwards.
type fakeDocker struct {
	mu       sync.Mutex
	volumes  []string
	creates  []string
	queries  []string
	requests []string
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := apiPath(r.URL.Path)
	owners := map[string]string{"mine": `{"Config":{"Labels":{"test.run":"` + runA + `"}}}`,
		"theirs": `{"Config":{"Labels":{"test.run":"` + runB + `"}}}`, "bosn": `{"Config":{"Labels":{}}}`}
	switch {
	case path == "/volumes/create":
		f.mu.Lock()
		f.volumes = append(f.volumes, string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	case path == "/containers/create":
		f.mu.Lock()
		f.creates = append(f.creates, r.URL.RawQuery+" "+string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"new"}`))
	case path == "/containers/json":
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	case strings.HasSuffix(path, "/attach"):
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString("echo:" + line)
		_ = rw.Flush()
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		doc, ok := owners[strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container"}`))
			return
		}
		_, _ = w.Write([]byte(doc))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short; t.TempDir() can be too long.
	dir, err := os.MkdirTemp("", "ap")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startProxyForTest(t *testing.T) (*http.Client, string, *fakeDocker) {
	t.Helper()
	dir := shortTempDir(t)
	upstream := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", upstream)
	require.NoError(t, err)
	docker := &fakeDocker{}
	server := &http.Server{Handler: docker, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	key, err := RunKey(runA)
	require.NoError(t, err)
	scope := Scope{RunID: runA, Key: key, Cgroup: "/t-" + key}
	proxy, err := startProxy(Config{RunLabel: "test.run", DockerUpstream: upstream, ProxyDir: filepath.Join(dir, "p"),
		SharedVolumes: []string{"act-toolcache"}}, scope)
	require.NoError(t, err)
	t.Cleanup(proxy.stop)
	info, err := os.Stat(proxy.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o666), info.Mode().Perm())
	return &http.Client{Transport: unixTransport(proxy.path)}, proxy.path, docker
}

func call(t *testing.T, c *http.Client, method, target, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://docker"+target, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestTheRunProxyScopesWhatARunCreatesAndSees(t *testing.T) {
	c, _, docker := startProxyForTest(t)
	code, _ := call(t, c, "POST", "/v1.47/containers/create?name=act-ci-job",
		`{"Image":"x","HostConfig":{"CgroupParent":"/","Binds":["act-ci-job-env:/env","act-toolcache:/opt/hostedtoolcache"]}}`)
	assert.Equal(t, http.StatusCreated, code)
	require.Len(t, docker.creates, 1)
	query, body, _ := strings.Cut(docker.creates[0], " ")
	assert.Equal(t, "name=act-ci-job-0a1b2c3d4e5f", query)
	var created struct {
		Labels     map[string]string
		HostConfig struct {
			CgroupParent string
			Binds        []string
		}
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	assert.Equal(t, runA, created.Labels["test.run"])
	assert.Equal(t, "/t-0a1b2c3d4e5f", created.HostConfig.CgroupParent)
	assert.Equal(t, []string{"act-ci-job-env-0a1b2c3d4e5f:/env", "act-toolcache:/opt/hostedtoolcache"}, created.HostConfig.Binds)
	require.Len(t, docker.volumes, 1, "the shared tool cache is neither keyed nor labelled")
	assert.JSONEq(t, `{"Name":"act-ci-job-env-0a1b2c3d4e5f","Labels":{"test.run":"`+runA+`"}}`, docker.volumes[0])

	_, _ = call(t, c, "GET", "/v1.47/containers/json?all=1", "")
	require.Len(t, docker.queries, 1)
	assert.Contains(t, docker.queries[0], "filters=")
	assert.Equal(t, map[string]any{"label": []any{"test.run=" + runA}}, filtersOf(t, docker.queries[0]))
}

func TestTheRunProxyHidesOtherRunsObjects(t *testing.T) {
	c, _, docker := startProxyForTest(t)
	code, _ := call(t, c, "POST", "/containers/mine/kill", "")
	assert.Equal(t, http.StatusOK, code)
	code, body := call(t, c, "DELETE", "/containers/theirs", "")
	assert.Equal(t, http.StatusNotFound, code)
	assert.JSONEq(t, `{"message":"No such container: theirs"}`, body)
	code, _ = call(t, c, "GET", "/containers/bosn/json", "")
	assert.Equal(t, http.StatusNotFound, code, "unlabelled containers are hidden")
	code, _ = call(t, c, "POST", "/containers/create", `{"HostConfig":{"NetworkMode":"container:theirs"}}`)
	assert.Equal(t, http.StatusForbidden, code)
	code, _ = call(t, c, "DELETE", "/containers/unknown", "")
	assert.Equal(t, http.StatusOK, code, "Docker answers for what it does not know")
	for _, request := range docker.requests {
		assert.NotEqual(t, "DELETE /containers/theirs", request)
	}
}

func TestTheRunProxyPassesHijackedStreams(t *testing.T) {
	_, path, _ := startProxyForTest(t)
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "POST /containers/mine/attach?stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_, err = fmt.Fprint(conn, "hello\n")
	require.NoError(t, err)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "echo:hello\n", line)
}

func TestReapRemovesOnlyRunSockets(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0a1b2c3d4e5f.sock", "engine.sock", "notes"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	reapSockets(dir)
	assert.NoFileExists(t, filepath.Join(dir, "0a1b2c3d4e5f.sock"))
	assert.FileExists(t, filepath.Join(dir, "engine.sock"))
	assert.FileExists(t, filepath.Join(dir, "notes"))
}
