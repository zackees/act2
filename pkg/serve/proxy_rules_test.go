package serve

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIPathStripsTheVersionPrefix(t *testing.T) {
	assert.Equal(t, "/containers/json", apiPath("/v1.47/containers/json"))
	assert.Equal(t, "/containers/json", apiPath("/containers/json"))
	assert.Equal(t, "/volumes/vx/y", apiPath("/volumes/vx/y"))
	assert.Equal(t, "/vx/containers", apiPath("/vx/containers"))
}

func filtersOf(t *testing.T, rawQuery string) map[string]any {
	t.Helper()
	query, err := url.ParseQuery(rawQuery)
	require.NoError(t, err)
	filters := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(query.Get("filters")), &filters))
	return filters
}

func TestListingsAndPrunesSeeOnlyTheRun(t *testing.T) {
	assert.Equal(t, scopeLabel, scopeOf("GET", "/v1.47/containers/json"))
	assert.Equal(t, scopeLabel, scopeOf("GET", "/volumes"))
	assert.Equal(t, scopeLabel, scopeOf("POST", "/images/prune"))
	assert.Equal(t, scopeBuildCache, scopeOf("POST", "/build/prune"))
	assert.Equal(t, forward, scopeOf("GET", "/networks"), "act resolves networks by listing them")
	assert.Equal(t, forward, scopeOf("GET", "/images/json"))

	q, err := scopeQuery(scopeLabel, "all=1", "l", "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"label": []any{"l=r"}}, filtersOf(t, q))
	q, err = scopeQuery(scopeLabel, "filters="+url.QueryEscape(`{"label":["a=b"],"status":["exited"]}`), "l", "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"label": []any{"a=b", "l=r"}, "status": []any{"exited"}}, filtersOf(t, q))
	q, err = scopeQuery(scopeLabel, "filters="+url.QueryEscape(`{"label":{"a=b":true}}`), "l", "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"label": map[string]any{"a=b": true, "l=r": true}}, filtersOf(t, q))
	q, err = scopeQuery(scopeBuildCache, "all=true", "l", "r")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"id": []any{"^$"}}, filtersOf(t, q))
	_, err = scopeQuery(scopeLabel, "filters=nope", "l", "r")
	assert.Error(t, err)
}

func TestContainerNamesGetTheRunKey(t *testing.T) {
	q, err := suffixName("name=act-ci-build", "0a1b2c3d4e5f")
	require.NoError(t, err)
	assert.Equal(t, "name=act-ci-build-0a1b2c3d4e5f", q)
	q, err = suffixName("name=x-0a1b2c3d4e5f", "0a1b2c3d4e5f")
	require.NoError(t, err)
	assert.Equal(t, "name=x-0a1b2c3d4e5f", q)
	q, err = suffixName("", "k")
	require.NoError(t, err)
	assert.Empty(t, q)
}

func TestRequestsNamingOneObjectAreFound(t *testing.T) {
	assert.Equal(t, &access{kind: kindContainer, id: "abc", write: true}, addressed("POST", "/v1.47/containers/abc/kill", ""))
	assert.Equal(t, &access{kind: kindContainer, id: "abc"}, addressed("GET", "/containers/abc/json", ""))
	assert.Nil(t, addressed("GET", "/containers/json", ""))
	assert.Nil(t, addressed("POST", "/containers/create", ""))
	assert.Equal(t, &access{kind: kindExec, id: "e1", write: true}, addressed("POST", "/exec/e1/start", ""))
	assert.Equal(t, &access{kind: kindNetwork, id: "n", write: true}, addressed("DELETE", "/networks/n", ""))
	assert.Equal(t, &access{kind: kindVolume, id: "v", write: true}, addressed("DELETE", "/volumes/v", ""))
	assert.Equal(t, &access{kind: kindImage, id: "ghcr.io/a/b:c", write: true}, addressed("DELETE", "/images/ghcr.io/a/b:c", ""))
	assert.Nil(t, addressed("GET", "/images/ghcr.io/a/b/json", ""), "images are shared")
	assert.Equal(t, &access{kind: kindContainer, id: "c1", write: true}, addressed("POST", "/commit", "container=c1"))
}

func TestVerdicts(t *testing.T) {
	read := access{kind: kindNetwork, id: "n"}
	write := access{kind: kindNetwork, id: "n", write: true}
	ctr := access{kind: kindContainer, id: "c"}
	assert.NoError(t, verdict(write, owner{missing: true}, "r"))
	assert.NoError(t, verdict(write, owner{run: "r"}, "r"))
	assert.Equal(t, 404, verdict(read, owner{run: "other"}, "r").(*refusal).status)
	assert.Equal(t, 404, verdict(ctr, owner{}, "r").(*refusal).status, "unlabelled containers are hidden")
	assert.NoError(t, verdict(read, owner{}, "r"), "shared networks stay readable")
	assert.Equal(t, 403, verdict(write, owner{}, "r").(*refusal).status)
}

func TestCreateReferences(t *testing.T) {
	refs, err := createReferences([]byte(`{"HostConfig":{"VolumesFrom":["a:ro"],"Links":["/b:alias"],"PidMode":"container:c",
		"IpcMode":"host","NetworkMode":"container:d"},"NetworkingConfig":{"EndpointsConfig":{"net1":{}}}}`))
	require.NoError(t, err)
	assert.ElementsMatch(t, []access{{kind: kindContainer, id: "a"}, {kind: kindContainer, id: "b"}, {kind: kindContainer, id: "c"},
		{kind: kindContainer, id: "d"}, {kind: kindNetwork, id: "net1"}}, refs)
	refs, err = createReferences([]byte(`{"HostConfig":{"NetworkMode":"bridge"}}`))
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestCreatesAreLabelledPinnedAndMapped(t *testing.T) {
	mapper := func(_ context.Context, name string) (string, error) { return name + "-k", nil }
	labels := map[string]string{"run": "r"}
	out, err := rewriteCreate(context.Background(), "container", []byte(`{"Image":"x","Labels":null,"HostConfig":{"CgroupParent":"/elsewhere","NanoCpus":12345678901234,
		"Binds":["vol:/a:ro","/host:/b"],"Mounts":[{"Type":"volume","Source":"m","Target":"/c"},{"Type":"bind","Source":"/h","Target":"/d"}]}}`),
		labels, "/run-cg", mapper)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	host := got["HostConfig"].(map[string]any)
	assert.Equal(t, map[string]any{"run": "r"}, got["Labels"])
	assert.Equal(t, "/run-cg", host["CgroupParent"])
	assert.Equal(t, []any{"vol-k:/a:ro", "/host:/b"}, host["Binds"])
	assert.Equal(t, "m-k", host["Mounts"].([]any)[0].(map[string]any)["Source"])
	assert.Equal(t, "/h", host["Mounts"].([]any)[1].(map[string]any)["Source"])
	assert.Contains(t, string(out), "12345678901234", "numbers stay exact")

	out, err = rewriteCreate(context.Background(), "volume", []byte(`{"Name":"v"}`), labels, "", mapper)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Name":"v-k","Labels":{"run":"r"}}`, string(out))
	out, err = rewriteCreate(context.Background(), "network", nil, labels, "", mapper)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Labels":{"run":"r"}}`, string(out))
	_, err = rewriteCreate(context.Background(), "network", []byte(`[]`), labels, "", mapper)
	assert.Error(t, err)
	_, err = rewriteCreate(context.Background(), "container", []byte(`{"HostConfig":{"Binds":["v:/x"]}}`), labels, "", func(context.Context, string) (string, error) {
		return "", assert.AnError
	})
	assert.Error(t, err, "a volume that cannot be prepared refuses the create")
}
