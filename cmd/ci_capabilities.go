package cmd

import (
	"encoding/json"
	"io"
)

// CI capabilities describe structured event contracts, not a successful run.
// The caller must still verify the binary pin and the actual execution receipt.
type ciCapabilities struct {
	SchemaVersion int      `json:"schema_version"`
	Producer      string   `json:"producer"`
	Version       string   `json:"version"`
	Capabilities  []string `json:"capabilities"`
}

func writeCICapabilities(output io.Writer, version string) error {
	return json.NewEncoder(output).Encode(ciCapabilities{
		SchemaVersion: 1,
		Producer:      "act2",
		Version:       version,
		Capabilities:  []string{"qualified-job-identity-v1", "step-stage-result-v1", "selected-job-outputs-v1", "cache-exact-delete-v1"},
	})
}
