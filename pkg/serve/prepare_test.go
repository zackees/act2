//go:build !windows

package serve

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinned() (ImagePin, string) {
	config := []byte(`{"os":"linux","architecture":"amd64","config":{"Env":["PATH=/bin"],"Cmd":["bash"]},"rootfs":{"type":"layers","diff_ids":["sha256:aa"]}}`)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d}}`,
		digestOf(config), len(config)))
	pin := ImagePin{Reference: "r@x", Tag: "t", Archive: "/a.tar", Platform: "linux/amd64", Manifest: manifest, Config: config}
	inspect := fmt.Sprintf(`[{"Id":%q,"Config":{"Env":["PATH=/bin"],"Entrypoint":null,"Cmd":["bash"],"User":"","WorkingDir":"","Volumes":null},
		"Descriptor":{"digest":%q,"mediaType":"application/vnd.oci.image.manifest.v1+json","size":%d},"RootFS":{"Type":"layers","Layers":["sha256:aa"]}}]`,
		digestOf(manifest), digestOf(manifest), len(manifest))
	return pin, inspect
}

func TestALoadedImageIsProvedAgainstItsPublisherBytes(t *testing.T) {
	pin, inspect := pinned()
	require.NoError(t, pin.Validate())
	id, err := VerifyLoadedImage([]byte(inspect), pin)
	require.NoError(t, err)
	assert.Equal(t, digestOf(pin.Manifest), id)
	for name, bad := range map[string]string{
		"other layers":  strings.Replace(inspect, `"sha256:aa"]`, `"sha256:bb"]`, 1),
		"other command": strings.Replace(inspect, `"Cmd":["bash"]`, `"Cmd":["sh"]`, 1),
		"a volume":      strings.Replace(inspect, `"Volumes":null`, `"Volumes":{"/v":{}}`, 1),
		"no descriptor": strings.Replace(inspect, `"Descriptor"`, `"Other"`, 1),
		"two images":    "[" + inspect[1:len(inspect)-1] + "," + inspect[1:],
	} {
		_, err := VerifyLoadedImage([]byte(bad), pin)
		assert.Error(t, err, name)
	}
	pin.Config = append(pin.Config, ' ')
	_, err = VerifyLoadedImage([]byte(inspect), pin)
	assert.Error(t, err, "config bytes that are not the manifest's")
	assert.Error(t, ImagePin{Tag: "t"}.Validate())
}

func write(t *testing.T, path, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
}

func saveServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	src, store := filepath.Join(dir, "volume"), filepath.Join(dir, "store")
	require.NoError(t, os.MkdirAll(src, 0o755))
	return &Server{cfg: Config{ToolCacheStore: store, ToolCacheVolume: "act-toolcache", ToolCacheMount: src}}, src, store
}

func TestSavingTheToolCacheKeepsOnlyCompletePristineInstallsOnce(t *testing.T) {
	s, src, store := saveServer(t)
	write(t, filepath.Join(src, "Python/3.11/x64/bin/python"), "py")
	write(t, filepath.Join(src, "uv/0.12/x86_64/uv"), "half-written: no marker")
	write(t, filepath.Join(store, "node/24/x64/kept"), "existing")
	write(t, filepath.Join(store, "node/24/x64.complete"), "")
	write(t, filepath.Join(src, "node/24/x64/new"), "must not replace the saved one")
	write(t, filepath.Join(src, "soldr-syslib/p/lib/1/slug/lib.so"), "so")
	past := time.Now().Add(-time.Minute)
	for _, f := range []string{"Python/3.11/x64/bin/python", "node/24/x64/new", "soldr-syslib/p/lib/1/slug/lib.so"} {
		require.NoError(t, os.Chtimes(filepath.Join(src, f), past, past))
	}
	write(t, filepath.Join(src, "Python/3.11/x64.complete"), "")
	write(t, filepath.Join(src, "node/24/x64.complete"), "")
	write(t, filepath.Join(src, "soldr-syslib/p/lib/1/slug/.complete"), "")
	// Changed after it completed: another repository's packages.
	write(t, filepath.Join(src, "go/1/x64/bin/go"), "go")
	write(t, filepath.Join(src, "go/1/x64.complete"), "")
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(src, "go/1/x64/bin/go"), later, later))
	// An abandoned stage is removed; a fresh one is kept.
	write(t, filepath.Join(store, ".saving-old/x"), "")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(store, ".saving-old"), old, old))

	result := s.saveToolcache(context.Background())
	require.True(t, result.Saved, result.Note)
	assert.FileExists(t, filepath.Join(store, "Python/3.11/x64/bin/python"))
	assert.FileExists(t, filepath.Join(store, "Python/3.11/x64.complete"))
	assert.NoDirExists(t, filepath.Join(store, "uv"), "an install without its marker is incomplete")
	assert.NoFileExists(t, filepath.Join(store, "node/24/x64/new"))
	assert.FileExists(t, filepath.Join(store, "soldr-syslib/p/lib/1/slug/lib.so"))
	assert.NoDirExists(t, filepath.Join(store, "go"), "a changed install is not shared")
	assert.NoDirExists(t, filepath.Join(store, ".saving-old"))
	again := s.saveToolcache(context.Background())
	assert.True(t, again.Saved)
}

func TestTheOverlayRecipeIsFrozenPerStoreAndTarget(t *testing.T) {
	recipe := OverlayRecipe("/store", "/target")
	assert.Contains(t, recipe, "store=/store")
	assert.Contains(t, recipe, "target=/target")
	assert.Contains(t, recipe, "generation=@GENERATION@")
	s := &Server{cfg: Config{Socket: filepath.Join(t.TempDir(), "s.sock"), ToolCacheStore: "/s", ToolCacheVolume: "v", ToolCacheMount: "/target"}}
	_, err := s.prepareToolcache(context.Background(), &ToolGeneration{ID: "g1", Store: "/store", OverlayRecipeSHA256: "sha256:00"})
	assert.ErrorContains(t, err, "differs from frozen producer")
	_, err = s.prepareToolcache(context.Background(), &ToolGeneration{ID: "../x", Store: "/store"})
	assert.Error(t, err)
}

func TestTheCacheBudgetPrunesCoordinatedNamespacesUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	act := filepath.Join(dir, "act")
	write(t, act, "#!/bin/sh\necho \"{\\\"namespace\\\":\\\"$5\\\"}\"\necho \"$@\" >> \"$(dirname \"$5\")/calls\"\n")
	require.NoError(t, os.Chmod(act, 0o755)) //nolint:gosec,nolintlint // a test executable
	root := filepath.Join(dir, "cache")
	write(t, filepath.Join(root, "0123456789abcdef/transfers.bolt"), "")
	write(t, filepath.Join(root, "fedcba9876543210/archive"), "")
	write(t, filepath.Join(root, "not-a-namespace/transfers.bolt"), "")
	budget := Budget{Root: root, Lock: ".lock", MaxBytes: 8, UnusedAge: time.Hour, MaxAge: 2 * time.Hour, Interval: time.Minute, MaxNamespaces: 64}
	pass := runBudgetPass(context.Background(), act, budget)
	assert.Equal(t, "ok", pass.Status, pass.Error)
	assert.Equal(t, 1, pass.Namespaces)
	assert.Equal(t, []string{"fedcba9876543210"}, pass.Skipped, "an uncoordinated namespace is never touched")
	require.Len(t, pass.Audits, 1)
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	require.NoError(t, err)
	assert.Equal(t, "cache prune --apply --cache-server-path "+filepath.Join(root, "0123456789abcdef")+
		" --cache-server-max-bytes 8 --cache-server-max-age 2h0m0s --cache-server-unused-age 1h0m0s --cache-server-gc-interval 1m0s\n", string(calls))

	holder := exec.Command("flock", "-x", filepath.Join(root, ".lock"), "sleep", "5") //nolint:gosec,nolintlint // a test lock holder
	require.NoError(t, holder.Start())
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	require.Eventually(t, func() bool { return runBudgetPass(context.Background(), act, budget).Status == "busy" }, 3*time.Second, 50*time.Millisecond)
}
