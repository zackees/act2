package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A run's Docker proxy. act, and every job container act gives the Docker
// socket to, reach the engine's dockerd only through the run's own socket,
// <proxy-dir>/<key>.sock, which the server opens at admission and removes at
// close. It forwards every request and response, except that:
//
//   - container, network and volume creates get the run's label; a container
//     is forced into the run's cgroup parent and gets the run's key on its
//     name; act's per-job volumes (act-*) get the key, and every named volume
//     a run uses is created with the label, so closing the run removes it;
//   - container and volume listings, and prunes, see only the run's objects;
//     a build-cache prune matches nothing;
//   - a request naming one object is forwarded only when the object is the
//     run's (another run's, and any unlabelled container, is a 404); a
//     container create cannot reach into another run's containers or networks.
//
// Hijacked streams (attach, exec start) and streamed responses pass through.

const (
	maxRewriteBody  = 8 << 20
	maxInspectBytes = 1 << 20
	inspectDeadline = 10 * time.Second
)

// runSocketName matches the sockets this server makes in its proxy directory.
var runSocketName = regexp.MustCompile(`^[0-9a-f]{12}\.sock$`)

type runProxy struct {
	path     string
	server   *http.Server
	upstream *http.Client
	label    string
	scope    Scope
	shared   map[string]bool
	reverse  *httputil.ReverseProxy
}

func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
}

// startProxy listens on the run's socket (mode 0666: job containers run as
// any user) and serves until stop.
func startProxy(cfg Config, scope Scope) (*runProxy, error) {
	if err := os.MkdirAll(cfg.ProxyDir, 0o711); err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.ProxyDir, scope.Key+".sock")
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec,nolintlint // job containers run as any user; the socket only reaches the run's own objects
		listener.Close()
		return nil, err
	}
	shared := map[string]bool{}
	for _, name := range cfg.SharedVolumes {
		shared[name] = true
	}
	p := &runProxy{
		path:     path,
		upstream: &http.Client{Transport: unixTransport(cfg.DockerUpstream), Timeout: inspectDeadline},
		label:    cfg.RunLabel,
		scope:    scope,
		shared:   shared,
		reverse: &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "http", "docker", "docker"
			},
			Transport:     unixTransport(cfg.DockerUpstream),
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				dockerError(w, http.StatusBadGateway, err.Error())
			},
		},
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *runProxy) stop() {
	if p == nil {
		return
	}
	_ = p.server.Close()
	_ = os.Remove(p.path)
}

func dockerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func (p *runProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := p.apply(r); err != nil {
		var refused *refusal
		if errors.As(err, &refused) {
			dockerError(w, refused.status, refused.message)
		} else {
			dockerError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	p.reverse.ServeHTTP(w, r)
}

// apply rewrites or checks r in place, or refuses it.
func (p *runProxy) apply(r *http.Request) error {
	if kind := createKind(r.Method, r.URL.Path); kind != "" {
		return p.applyCreate(kind, r)
	}
	query, err := scopeQuery(scopeOf(r.Method, r.URL.Path), r.URL.RawQuery, p.label, p.scope.RunID)
	if err != nil {
		return refuse(http.StatusBadRequest, "%v", err)
	}
	r.URL.RawQuery = query
	if a := addressed(r.Method, r.URL.Path, r.URL.RawQuery); a != nil {
		if err := p.check(r.Context(), *a); err != nil {
			var refused *refusal
			if errors.As(err, &refused) {
				return err
			}
			// Fail closed: an object the proxy cannot resolve is not forwarded.
			return refuse(http.StatusInternalServerError, "act serve docker proxy could not resolve %s: %v", a.id, err)
		}
	}
	return nil
}

func (p *runProxy) applyCreate(kind string, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRewriteBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxRewriteBody {
		return refuse(http.StatusInternalServerError, "act serve docker proxy refused the %s create: body too large", kind)
	}
	labels := map[string]string{p.label: p.scope.RunID}
	cgroup := ""
	if kind == "container" {
		cgroup = p.scope.Cgroup
	}
	rewritten, err := rewriteCreate(r.Context(), kind, body, labels, cgroup, p.mapVolume)
	if err != nil {
		// Never forward an unaccounted create.
		return refuse(http.StatusInternalServerError, "act serve docker proxy refused the %s create: %v", kind, err)
	}
	if kind == "container" {
		refs, err := createReferences(rewritten)
		if err != nil {
			return refuse(http.StatusInternalServerError, "act serve docker proxy refused the container create: %v", err)
		}
		for _, ref := range refs {
			if err := p.check(r.Context(), ref); err != nil {
				// 403, not 404: the Docker CLI reads a 404 on create as a
				// missing image and starts pulling.
				return refuse(http.StatusForbidden, "act serve docker proxy refused the container create: %v", err)
			}
		}
		if r.URL.RawQuery, err = suffixName(r.URL.RawQuery, p.scope.Key); err != nil {
			return refuse(http.StatusBadRequest, "%v", err)
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.TransferEncoding = nil
	r.Header.Del("Transfer-Encoding")
	r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

// volumeName gives act's per-job volumes (act-<workflow>-<job>-...) the
// run's key; act names them after the workflow and job only.
func (p *runProxy) volumeName(name string) string {
	if !p.shared[name] && strings.HasPrefix(name, "act-") && !strings.HasSuffix(name, p.scope.Key) {
		return name + "-" + p.scope.Key
	}
	return name
}

// mapVolume maps name and creates the volume with the run's label first (a
// volume Docker would create implicitly would carry none). Docker returns an
// existing volume of that name unchanged.
func (p *runProxy) mapVolume(ctx context.Context, name string) (string, error) {
	mapped := p.volumeName(name)
	if p.shared[mapped] {
		return mapped, nil
	}
	body, _ := json.Marshal(map[string]any{"Name": mapped, "Labels": map[string]string{p.label: p.scope.RunID}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/volumes/create", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.upstream.Do(req)
	if err != nil {
		return "", fmt.Errorf("volume %s not created: %w", mapped, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("volume %s not created: %s", mapped, resp.Status)
	}
	return mapped, nil
}

// inspect resolves who owns one object.
func (p *runProxy) inspect(ctx context.Context, kind objectKind, id string) (owner, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+kind.inspectPath(id), nil)
	if err != nil {
		return owner{}, "", err
	}
	resp, err := p.upstream.Do(req)
	if err != nil {
		return owner{}, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return owner{missing: true}, "", nil
	case http.StatusOK:
	default:
		return owner{}, "", fmt.Errorf("%s inspect answered %d", kind.noun(), resp.StatusCode)
	}
	var doc struct {
		Labels      map[string]string                   `json:"Labels"`
		Config      *struct{ Labels map[string]string } `json:"Config"`
		ContainerID string                              `json:"ContainerID"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInspectBytes)).Decode(&doc); err != nil {
		return owner{}, "", fmt.Errorf("%s inspect returned malformed JSON", kind.noun())
	}
	labels := doc.Labels
	switch kind {
	case kindExec:
		return owner{}, doc.ContainerID, nil
	case kindContainer, kindImage:
		labels = nil
		if doc.Config != nil {
			labels = doc.Config.Labels
		}
	}
	return owner{run: labels[p.label]}, "", nil
}

// check refuses a unless the run may make it. An exec instance is owned by
// its container.
func (p *runProxy) check(ctx context.Context, a access) error {
	o, container, err := p.inspect(ctx, a.kind, a.id)
	if err == nil && a.kind == kindExec && !o.missing {
		o, _, err = p.inspect(ctx, kindContainer, container)
	}
	if err != nil {
		return err
	}
	return verdict(a, o, p.scope.RunID)
}

// reapSockets removes run sockets a previous server left in dir.
func reapSockets(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if runSocketName.MatchString(entry.Name()) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
