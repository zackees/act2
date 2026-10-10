package serve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
)

// ImagePin names one runner image and the publisher bytes that prove it.
type ImagePin struct {
	// Reference is the pull reference, by digest.
	Reference string `json:"reference"`
	// Tag is the local name the image is loaded under.
	Tag string `json:"tag"`
	// Archive is the image tar kept in the shared cache, loaded instead of
	// pulling when present.
	Archive  string `json:"archive"`
	Platform string `json:"platform"`
	// Manifest and Config are the publisher's manifest and config bytes;
	// their digests are the pins.
	Manifest []byte `json:"manifest"`
	Config   []byte `json:"config"`
}

// ImageState is a runner image the server loaded and proved.
type ImageState struct {
	Tag string `json:"tag"`
	ID  string `json:"id"`
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Validate refuses a pin the server cannot act on.
func (p ImagePin) Validate() error {
	switch {
	case p.Reference == "" || p.Tag == "" || p.Archive == "" || p.Platform == "":
		return errors.New("image: reference, tag, archive and platform are required")
	case len(p.Manifest) == 0 || len(p.Manifest) > 1<<20 || len(p.Config) == 0 || len(p.Config) > 1<<20:
		return errors.New("image: manifest and config bytes are required (at most 1 MiB each)")
	}
	return nil
}

type manifestDoc struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
		MediaType string `json:"mediaType"`
	} `json:"config"`
}

type configDoc struct {
	OS           string         `json:"os"`
	Architecture string         `json:"architecture"`
	Config       map[string]any `json:"config"`
	RootFS       struct {
		Type    string `json:"type"`
		DiffIDs []any  `json:"diff_ids"`
	} `json:"rootfs"`
}

type inspectDoc struct {
	ID         string         `json:"Id"`
	Config     map[string]any `json:"Config"`
	Descriptor struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int    `json:"size"`
	} `json:"Descriptor"`
	RootFS struct {
		Type   string `json:"Type"`
		Layers []any  `json:"Layers"`
	} `json:"RootFS"`
}

func normalized(v any, empty any) any {
	if v == nil {
		return empty
	}
	return v
}

// VerifyLoadedImage proves a `docker image inspect` document (containerd
// image store metadata) is the pinned image: its manifest descriptor, config,
// rootfs and execution config match the publisher bytes. Classic stores
// without a manifest descriptor fail closed. It returns the image ID.
func VerifyLoadedImage(inspect []byte, pin ImagePin) (string, error) {
	m, c, err := pinnedDocs(pin)
	if err != nil {
		return "", err
	}
	var records []inspectDoc
	decoder := json.NewDecoder(bytes.NewReader(inspect))
	decoder.UseNumber()
	if err := decoder.Decode(&records); err != nil || len(records) != 1 {
		return "", errors.New("expected exactly one imported image")
	}
	image := records[0]
	if !sameExecution(image.Config, c.Config) {
		return "", errors.New("imported image execution config differs from pinned config")
	}
	if !sameIdentity(image, pin, m, c) {
		return "", errors.New("imported manifest, config or rootfs identity not established")
	}
	return image.ID, nil
}

// pinnedDocs parses the publisher bytes and checks the manifest binds a
// Linux amd64 config.
func pinnedDocs(pin ImagePin) (manifestDoc, configDoc, error) {
	var m manifestDoc
	var c configDoc
	if json.Unmarshal(pin.Manifest, &m) != nil || json.Unmarshal(pin.Config, &c) != nil {
		return m, c, errors.New("pinned image proof bytes are not JSON")
	}
	manifestTypes := map[string]bool{"application/vnd.oci.image.manifest.v1+json": true, "application/vnd.docker.distribution.manifest.v2+json": true}
	configTypes := map[string]bool{"application/vnd.oci.image.config.v1+json": true, "application/vnd.docker.container.image.v1+json": true}
	if m.SchemaVersion != 2 || !manifestTypes[m.MediaType] || !configTypes[m.Config.MediaType] ||
		m.Config.Digest != digestOf(pin.Config) || m.Config.Size != len(pin.Config) ||
		c.OS != "linux" || c.Architecture != "amd64" || c.RootFS.Type != "layers" {
		return m, c, errors.New("imported image manifest does not bind pinned Linux config")
	}
	return m, c, nil
}

// sameExecution compares what a container of the image would run.
func sameExecution(actual, pinned map[string]any) bool {
	if pinned == nil {
		pinned = map[string]any{}
	}
	for _, field := range []string{"Env", "Entrypoint", "Cmd"} {
		got := normalized(actual[field], []any{})
		if _, ok := got.([]any); !ok || !reflect.DeepEqual(got, normalized(pinned[field], []any{})) {
			return false
		}
	}
	for _, field := range []string{"User", "WorkingDir"} {
		got, ok := normalized(actual[field], "").(string)
		want, _ := normalized(pinned[field], "").(string)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// sameIdentity checks the image ID, manifest descriptor, rootfs and that the
// image declares no volumes.
func sameIdentity(image inspectDoc, pin ImagePin, m manifestDoc, c configDoc) bool {
	manifest := digestOf(pin.Manifest)
	volumes, isMap := image.Config["Volumes"].(map[string]any)
	noVolumes := image.Config["Volumes"] == nil || (isMap && len(volumes) == 0)
	return imageID.MatchString(image.ID) && (image.ID == manifest || image.ID == digestOf(pin.Config)) &&
		image.Descriptor.Digest == manifest && image.Descriptor.MediaType == m.MediaType &&
		image.Descriptor.Size == len(pin.Manifest) && image.RootFS.Type == "layers" &&
		reflect.DeepEqual(image.RootFS.Layers, c.RootFS.DiffIDs) && noVolumes
}
