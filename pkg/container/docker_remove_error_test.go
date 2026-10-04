//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovePreservesDockerErrorAndContainerIdentity(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.54")
			w.WriteHeader(http.StatusOK)
			return
		}
		assert.Equal(t, http.MethodDelete, r.Method)
		if attempts.Add(1) > 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"removal blocked by storage"}`))
	}))
	defer server.Close()
	cli, err := client.New(client.WithHost(server.URL))
	require.NoError(t, err)
	defer cli.Close()
	cr := &containerReference{cli: cli, id: "cleanup-test-container"}
	err = cr.remove()(context.Background())
	require.ErrorContains(t, err, "removal blocked by storage")
	assert.Equal(t, "cleanup-test-container", cr.id, "failed removal must retain the identity for diagnostics or retry")
	require.NoError(t, cr.remove()(context.Background()))
	assert.Empty(t, cr.id, "confirmed removal clears the retained identity")
	assert.Equal(t, int32(2), attempts.Load())
	require.NoError(t, cr.remove()(context.Background()))
	assert.Equal(t, int32(2), attempts.Load(), "an absent container needs no Docker request")
}
