package container

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostEnvironmentSeparateStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	var stdout, stderr bytes.Buffer
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	e := &HostEnvironment{
		Path: t.TempDir(), StdOut: &stdout, StdErr: &stderr, SeparateStreams: true,
	}
	if err := e.Exec([]string{shell, "-c", "printf out; printf err >&2"}, nil, "", "")(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// Type assert HostEnvironment implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &HostEnvironment{}

func TestCopyDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-host-env-*")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)
	ctx := context.Background()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0700)
	_ = os.MkdirAll(e.TmpDir, 0700)
	_ = os.MkdirAll(e.ToolCache, 0700)
	_ = os.MkdirAll(e.ActPath, 0700)
	err = e.CopyDir(e.Workdir, e.Path, true)(ctx)
	assert.NoError(t, err)
}

func TestGetContainerArchive(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-host-env-*")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)
	ctx := context.Background()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0700)
	_ = os.MkdirAll(e.TmpDir, 0700)
	_ = os.MkdirAll(e.ToolCache, 0700)
	_ = os.MkdirAll(e.ActPath, 0700)
	expectedContent := []byte("sdde/7sh")
	err = os.WriteFile(filepath.Join(e.Path, "action.yml"), expectedContent, 0600)
	assert.NoError(t, err)
	archive, err := e.GetContainerArchive(ctx, e.Path)
	assert.NoError(t, err)
	defer archive.Close()
	reader := tar.NewReader(archive)
	h, err := reader.Next()
	assert.NoError(t, err)
	assert.Equal(t, "action.yml", h.Name)
	content, err := io.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, expectedContent, content)
	_, err = reader.Next()
	assert.ErrorIs(t, err, io.EOF)
}
