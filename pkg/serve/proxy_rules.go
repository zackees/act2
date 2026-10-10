package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The request rules of a run's Docker proxy: which requests are rewritten,
// scoped to the run, or checked against the owner of the object they name.
// They are pure functions of the request, so they are tested without Docker.

// refusal is a request the proxy answers itself, in Docker's error shape.
type refusal struct {
	status  int
	message string
}

func (r *refusal) Error() string { return r.message }

func refuse(status int, format string, args ...any) error {
	return &refusal{status: status, message: fmt.Sprintf(format, args...)}
}

// apiPath strips Docker's optional /v1.NN prefix.
func apiPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/v")
	if !ok {
		return path
	}
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || strings.Trim(rest[:slash], "0123456789.") != "" {
		return path
	}
	return rest[slash:]
}

// createKind names the create call a request makes, if any.
func createKind(method, path string) string {
	if method != "POST" {
		return ""
	}
	switch apiPath(path) {
	case "/containers/create":
		return "container"
	case "/networks/create":
		return "network"
	case "/volumes/create":
		return "volume"
	}
	return ""
}

type listScope int

const (
	forward listScope = iota
	// scopeLabel adds the run's label to the request's filters, so a listing
	// or a prune sees only the run's own objects.
	scopeLabel
	// scopeBuildCache narrows a build-cache prune to nothing: BuildKit takes
	// no label filter and its cache is the engine's.
	scopeBuildCache
)

// scopeOf classifies a request that is not a create. Image and network
// listings stay whole: images carry no run label, and act resolves networks
// by listing them.
func scopeOf(method, path string) listScope {
	switch p := apiPath(path); {
	case method == "GET" && (p == "/containers/json" || p == "/volumes"):
		return scopeLabel
	case method == "POST" && (p == "/containers/prune" || p == "/volumes/prune" || p == "/networks/prune" || p == "/images/prune"):
		return scopeLabel
	case method == "POST" && p == "/build/prune":
		return scopeBuildCache
	}
	return forward
}

func decodeFilters(query url.Values) (map[string]any, error) {
	filters := map[string]any{}
	if raw := strings.TrimSpace(query.Get("filters")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			return nil, errors.New("malformed request filters")
		}
	}
	return filters, nil
}

func encodeFilters(query url.Values, filters map[string]any) (string, error) {
	encoded, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	query.Set("filters", string(encoded))
	return query.Encode(), nil
}

// scopeQuery applies scope to a raw query string.
func scopeQuery(scope listScope, rawQuery, label, runID string) (string, error) {
	if scope == forward {
		return rawQuery, nil
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errors.New("malformed request query")
	}
	filters, err := decodeFilters(query)
	if err != nil {
		return "", err
	}
	if scope == scopeBuildCache {
		// BuildKit matches id as a regular expression; ^$ matches no record.
		filters["id"] = []any{"^$"}
		return encodeFilters(query, filters)
	}
	want := label + "=" + runID
	switch labels := filters["label"].(type) {
	case []any:
		filters["label"] = append(labels, want)
	case map[string]any:
		// Docker also accepts the legacy {"k=v": true} map.
		labels[want] = true
	default:
		filters["label"] = []any{want}
	}
	return encodeFilters(query, filters)
}

// suffixName gives a container create's ?name= the run's key, so two runs
// of one workflow never share (or force-remove) a container name.
func suffixName(rawQuery, key string) (string, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errors.New("malformed request query")
	}
	name := query.Get("name")
	if name == "" || strings.HasSuffix(name, "-"+key) {
		return rawQuery, nil
	}
	query.Set("name", name+"-"+key)
	return query.Encode(), nil
}

type objectKind int

const (
	kindContainer objectKind = iota
	kindExec
	kindNetwork
	kindVolume
	kindImage
)

func (k objectKind) noun() string {
	return [...]string{"container", "exec instance", "network", "volume", "image"}[k]
}

func (k objectKind) inspectPath(id string) string {
	id = url.PathEscape(id)
	return [...]string{"/containers/" + id + "/json", "/exec/" + id + "/json", "/networks/" + id, "/volumes/" + id, "/images/" + id + "/json"}[k]
}

// access is one object a request names, and whether the request changes it.
type access struct {
	kind  objectKind
	id    string
	write bool
}

// collectionRoutes share a prefix with object routes.
var collectionRoutes = map[string]bool{"json": true, "create": true, "prune": true, "search": true, "load": true, "get": true}

// addressed is the object a request names, if any. Image reads, pushes and
// tags are forwarded: images are shared and carry no run label.
func addressed(method, path, rawQuery string) *access {
	p := strings.TrimPrefix(apiPath(path), "/")
	write := method != "GET" && method != "HEAD"
	parts := strings.SplitN(p, "/", 3)
	at := func(kind objectKind, id string) *access {
		if id == "" || collectionRoutes[id] {
			return nil
		}
		return &access{kind: kind, id: id, write: write}
	}
	if len(parts) == 1 {
		if parts[0] == "commit" {
			query, _ := url.ParseQuery(rawQuery)
			if c := query.Get("container"); c != "" {
				return &access{kind: kindContainer, id: c, write: true}
			}
		}
		return nil
	}
	switch parts[0] {
	case "containers":
		return at(kindContainer, parts[1])
	case "exec":
		return at(kindExec, parts[1])
	case "networks":
		return at(kindNetwork, parts[1])
	case "volumes":
		if len(parts) == 2 {
			return at(kindVolume, parts[1])
		}
	case "images":
		// Image names contain slashes: everything after /images/ is one.
		if method == "DELETE" {
			return at(kindImage, strings.TrimPrefix(p, "images/"))
		}
	}
	return nil
}

// owner is who owns an inspected object: "" for an unlabelled one.
type owner struct {
	missing bool
	run     string
}

// verdict decides whether runID may make a to an object o owns. Another
// run's object, and any unlabelled container, is answered with Docker's own
// 404, so a job cannot learn it exists. Shared unlabelled networks, volumes
// and images stay readable, but a write to them is refused.
func verdict(a access, o owner, runID string) error {
	switch {
	case o.missing || (o.run != "" && o.run == runID):
		return nil
	case o.run != "", a.kind == kindContainer || a.kind == kindExec:
		return refuse(404, "No such %s: %s", a.kind.noun(), a.id)
	case !a.write:
		return nil
	}
	return refuse(403, "act serve docker proxy refused changing %s %s: it was not created by this run", a.kind.noun(), a.id)
}

// builtinNetworks name no network object.
var builtinNetworks = map[string]bool{"": true, "default": true, "bridge": true, "host": true, "none": true}

type createRefs struct {
	HostConfig *struct {
		VolumesFrom []string `json:"VolumesFrom"`
		Links       []string `json:"Links"`
		NetworkMode string   `json:"NetworkMode"`
		PidMode     string   `json:"PidMode"`
		IpcMode     string   `json:"IpcMode"`
	} `json:"HostConfig"`
	NetworkingConfig *struct {
		EndpointsConfig map[string]json.RawMessage `json:"EndpointsConfig"`
	} `json:"NetworkingConfig"`
}

// createReferences is every object a container create reaches into, as
// reads: its VolumesFrom, Links, container: modes and networks.
func createReferences(body []byte) ([]access, error) {
	var refs createRefs
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("container create body: %w", err)
	}
	var out []access
	read := func(kind objectKind, id string) {
		if id != "" {
			out = append(out, access{kind: kind, id: id})
		}
	}
	var networks []string
	if refs.NetworkingConfig != nil {
		for name := range refs.NetworkingConfig.EndpointsConfig {
			networks = append(networks, name)
		}
	}
	if h := refs.HostConfig; h != nil {
		for _, source := range h.VolumesFrom {
			read(kindContainer, strings.Split(source, ":")[0])
		}
		for _, link := range h.Links {
			read(kindContainer, strings.TrimPrefix(strings.Split(link, ":")[0], "/"))
		}
		for _, mode := range []string{h.PidMode, h.IpcMode} {
			if c, ok := strings.CutPrefix(mode, "container:"); ok {
				read(kindContainer, c)
			}
		}
		if c, ok := strings.CutPrefix(h.NetworkMode, "container:"); ok {
			read(kindContainer, c)
		} else {
			networks = append(networks, h.NetworkMode)
		}
	}
	for _, n := range networks {
		if !builtinNetworks[n] {
			read(kindNetwork, n)
		}
	}
	return out, nil
}

// decodeObject parses a JSON object, keeping numbers exact.
func decodeObject(body []byte) (map[string]any, error) {
	object := map[string]any{}
	if len(bytes.TrimSpace(body)) == 0 {
		return object, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	if object == nil {
		return nil, errors.New("body is not a JSON object")
	}
	return object, nil
}

// child returns object[key] as an object, creating it when absent or null.
func child(object map[string]any, key string) (map[string]any, error) {
	switch value := object[key].(type) {
	case nil:
		created := map[string]any{}
		object[key] = created
		return created, nil
	case map[string]any:
		return value, nil
	}
	return nil, fmt.Errorf("%s is not an object", key)
}

// isVolumeName: a bind source without a slash is a named volume.
func isVolumeName(source string) bool {
	return source != "" && !strings.ContainsAny(source, `/\`)
}

// volumeMapper maps a named volume to the one a run attaches.
type volumeMapper func(ctx context.Context, name string) (string, error)

// rewriteCreate labels one create body, pins a container's cgroup parent and
// maps its named volumes.
func rewriteCreate(ctx context.Context, kind string, body []byte, labels map[string]string, cgroupParent string, mapVolume volumeMapper) ([]byte, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	existing, err := child(object, "Labels")
	if err != nil {
		return nil, err
	}
	for k, v := range labels {
		existing[k] = v
	}
	switch kind {
	case "container":
		host, err := child(object, "HostConfig")
		if err != nil {
			return nil, err
		}
		if cgroupParent != "" {
			// Whatever the caller asked for: act's options, a workflow's
			// container.options, a service container.
			host["CgroupParent"] = cgroupParent
		}
		if err := mapContainerVolumes(ctx, host, mapVolume); err != nil {
			return nil, err
		}
	case "volume":
		if name, ok := object["Name"].(string); ok && name != "" {
			if object["Name"], err = mapVolume(ctx, name); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(object)
}

// mapContainerVolumes maps the named volumes in Binds ("name:/target[:opts]")
// and volume Mounts.
func mapContainerVolumes(ctx context.Context, host map[string]any, mapVolume volumeMapper) error {
	if binds, ok := host["Binds"].([]any); ok {
		for i, bind := range binds {
			text, ok := bind.(string)
			if !ok {
				continue
			}
			parts := strings.SplitN(text, ":", 2)
			if !isVolumeName(parts[0]) {
				continue
			}
			mapped, err := mapVolume(ctx, parts[0])
			if err != nil {
				return err
			}
			parts[0] = mapped
			binds[i] = strings.Join(parts, ":")
		}
	}
	if mounts, ok := host["Mounts"].([]any); ok {
		for _, entry := range mounts {
			mount, ok := entry.(map[string]any)
			if !ok || mount["Type"] != "volume" {
				continue
			}
			source, _ := mount["Source"].(string)
			if source == "" {
				continue
			}
			mapped, err := mapVolume(ctx, source)
			if err != nil {
				return err
			}
			mount["Source"] = mapped
		}
	}
	return nil
}
