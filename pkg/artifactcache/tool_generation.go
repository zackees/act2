package artifactcache

import "context"

// ToolGenerationInstall places a verified closed install at its tool-cache path.
type ToolGenerationInstall struct {
	Path     string `json:"path"`
	ObjectID string `json:"object_id"`
}

// ToolGenerationSpec is a complete desired generation, not an in-place update.
type ToolGenerationSpec struct {
	SchemaVersion int                     `json:"schema_version"`
	Installs      []ToolGenerationInstall `json:"installs"`
}

// PublishToolGeneration assembles immutable install objects without copying file
// data. The caller must expose the resulting tree only as a read-only lower;
// reader protection and retirement are separate requirements.
func PublishToolGeneration(ctx context.Context, root string, spec ToolGenerationSpec, maxBytes int64) ToolSnapshotReport {
	return publishToolGeneration(ctx, root, spec, maxBytes)
}
