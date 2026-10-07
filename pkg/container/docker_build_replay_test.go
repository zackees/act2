//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestBuildContextReplayLimitAndCleanup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	_, err := newReplayBuildContextLimit(strings.NewReader("overflow"), 4)
	require.Error(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
	replay, err := newReplayBuildContextLimit(strings.NewReader("exact"), 5)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		reader, err := replay.reader()
		require.NoError(t, err)
		contents, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, "exact", string(contents))
	}
	replay.close()
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
func TestBuildContextReplayReadFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	_, err := newReplayBuildContextLimit(failingBuildContext{}, 100)
	require.ErrorContains(t, err, "source failed")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

type failingBuildContext struct{}

func (failingBuildContext) Read([]byte) (int, error) { return 0, errors.New("source failed") }
func TestFinalBuildPlatformResolution(t *testing.T) {
	for _, test := range []struct {
		name, dockerfile, architecture string
		known                          bool
	}{
		{"ordinary", "FROM local:latest\n", "amd64", true},
		{"scratch", "FROM scratch\n", "amd64", true},
		{"override", "FROM --platform=linux/arm64 local:latest\n", "arm64", true},
		{"arg-default", "ARG P=linux/arm64\nFROM --platform=${P} local:latest\n", "arm64", true},
		{"arg-expansion", "ARG ARCH=arm64\nARG P=linux/${ARCH}\nFROM --platform=$P local:latest\n", "arm64", true},
		{"named-inheritance", "FROM --platform=linux/arm64 local:latest AS BUILD\nFROM build\n", "arm64", true},
		{"final-reset", "FROM --platform=linux/arm64 local:latest AS BUILD\nFROM scratch\n", "amd64", true},
		{"unresolved", "FROM --platform=$UNSET local:latest\n", "", false},
		{"invalid-platform", "FROM --platform=invalid local:latest\n", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := resolveFinalBuildPlatform([]byte(test.dockerfile), specs.Platform{OS: "linux", Architecture: "amd64"})
			require.Equal(t, test.known, got.known)
			if test.known {
				require.Equal(t, test.architecture, got.platform.Architecture)
			}
		})
	}
}

func TestBuildContextFiniteReaderContract(t *testing.T) {
	for _, source := range []io.Reader{bytes.NewBufferString("finite"), bytes.NewReader([]byte("finite")), strings.NewReader("finite")} {
		wrapped, err := interruptibleBuildInput(source)
		require.NoError(t, err)
		contents, err := io.ReadAll(wrapped)
		require.NoError(t, err)
		require.Equal(t, "finite", string(contents))
		require.NoError(t, wrapped.Close())
	}
	_, err := interruptibleBuildInput(failingBuildContext{})
	require.ErrorContains(t, err, "must implement Close")
}
