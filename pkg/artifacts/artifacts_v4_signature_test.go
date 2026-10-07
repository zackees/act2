package artifacts

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The pinned stock upload-artifact Azure client drops a raw trailing '='
// from signature values when serializing its block and block-list requests.
// Escaping the value must preserve the original signed bytes on both paths.
func TestV4SignatureSurvivesBlobClientQuerySerialization(t *testing.T) {
	route := artifactV4Routes{AppURL: "localhost", prefix: ArtifactV4RouteBase}
	for _, endpoint := range []string{"UploadArtifact", "DownloadArtifact"} {
		t.Run(endpoint, func(t *testing.T) {
			target, err := url.Parse(route.buildArtifactURL(endpoint, "reuse-check", 1))
			require.NoError(t, err)
			pairs := strings.Split(target.RawQuery, "&")
			for i, pair := range pairs {
				parts := strings.Split(pair, "=")
				if len(parts) > 1 {
					pairs[i] = parts[0] + "=" + parts[1]
				}
			}
			target.RawQuery = strings.Join(pairs, "&") + "&comp=block"
			response := httptest.NewRecorder()
			task, name, ok := route.verifySignature(&ArtifactContext{
				Req:  httptest.NewRequest(http.MethodPut, target.String(), nil),
				Resp: response,
			}, endpoint)
			require.True(t, ok, "stock client serialization invalidated the signature")
			require.Equal(t, int64(1), task)
			require.Equal(t, "reuse-check", name)
		})
	}
}
