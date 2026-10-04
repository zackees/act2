package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nektos/act/pkg/artifactcache"
)

func TestCachePolicyFlags(t *testing.T) {
	input := &Input{}
	cmd := createRootCommand(context.Background(), input, "test")
	require.Equal(t, artifactcache.DefaultPolicy(), input.cachePolicy)
	require.NoError(t, cmd.ParseFlags([]string{
		"--cache-server-cohort-root", "/tmp/cohort",
		"--cache-server-cohort-max-bytes", "2147483648",
		"--cache-server-max-bytes", "1073741824",
		"--cache-server-max-age", "48h",
		"--cache-server-unused-age", "24h",
		"--cache-server-gc-interval", "1m",
	}))
	require.Equal(t, artifactcache.Policy{CohortRoot: "/tmp/cohort", CohortMaxBytes: 2147483648, MaxBytes: 1073741824, MaxAge: 48 * time.Hour,
		UnusedAge: 24 * time.Hour, GCInterval: time.Minute}, input.cachePolicy)
	require.NoError(t, input.cachePolicy.Validate())
}
