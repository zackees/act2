//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/nektos/act/pkg/common"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Replay is bounded independently of archive expansion. Files remain private
// and are removed on every return; a limit breach is an error, never EOF.
const maxBuildContextBytes int64 = 8 << 30
const maxBuildDockerfileBytes int64 = 4 << 20

type replayBuildContext struct{ file *os.File }

func newReplayBuildContextLimit(source io.Reader, limit int64) (*replayBuildContext, error) {
	return newReplayBuildContextLimitContext(context.Background(), source, limit)
}
func newReplayBuildContextLimitContext(ctx context.Context, source io.Reader, limit int64) (*replayBuildContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "act-build-context-*")
	if err != nil {
		return nil, err
	}
	replay := &replayBuildContext{file: file}
	stop := context.AfterFunc(ctx, func() {
		if cancelSource, ok := source.(interface{ CancelRead() error }); ok {
			_ = cancelSource.CancelRead()
		} else if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stop()
	count, err := io.Copy(file, io.LimitReader(contextBuildReader{ctx: ctx, source: source}, limit+1))
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && count > limit {
		err = fmt.Errorf("Docker build context exceeds %d byte replay limit", limit)
	}
	if err != nil {
		replay.close()
		return nil, err
	}
	return replay, nil
}
func (replay *replayBuildContext) close() {
	name := replay.file.Name()
	_ = replay.file.Close()
	_ = os.Remove(name)
}
func (replay *replayBuildContext) reader() (io.Reader, error) {
	_, err := replay.file.Seek(0, io.SeekStart)
	// The Docker HTTP transport owns request bodies; keep the replay file alive
	// for an identical retry and close it only through replay.close.
	return struct{ io.Reader }{replay.file}, err
}
func (replay *replayBuildContext) dockerfile(name string) ([]byte, error) {
	source, err := replay.reader()
	if err != nil {
		return nil, err
	}
	decoded, err := compression.DecompressStream(source)
	if err != nil {
		return nil, err
	}
	defer decoded.Close()
	guard := &buildArchiveReader{source: decoded, limits: buildArchiveLimits{bytes: maxBuildContextBytes, headers: 1000000, metadataBytes: 1 << 20, metadataTotal: 8 << 20}}
	reader := tar.NewReader(guard)
	var contents []byte
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return contents, guard.finish()
		}
		if err != nil {
			return nil, err
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return nil, errors.New("unsupported sparse Docker build context")
			}
		}
		if header.Size != guard.headerSize {
			return nil, errors.New("Docker build archive metadata changes raw entry size")
		}
		if path.Clean(header.Name) != path.Clean(strings.ReplaceAll(name, "\\", "/")) {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size > maxBuildDockerfileBytes {
			return nil, errors.New("Dockerfile must be one regular file within the 4 MiB inspection limit")
		}
		found = true
		contents, err = io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
	}
}
func buildPlatformMatches(image client.ImageInspectResult, expected specs.Platform) bool {
	return image.Os == expected.OS && image.Architecture == expected.Architecture && (expected.Variant == "" || image.Variant == expected.Variant)
}
func inspectBuiltImage(ctx context.Context, cli client.APIClient, name string, expected specs.Platform) (client.ImageInspectResult, error) {
	image, err := cli.ImageInspect(ctx, name)
	if err != nil || versions.LessThan(cli.ClientVersion(), "1.49") {
		return image, err
	}
	return cli.ImageInspect(ctx, name, client.ImageInspectWithPlatform(&expected))
}
func executeClassicBuild(ctx context.Context, cli client.APIClient, source io.Reader, options client.ImageBuildOptions) error {
	response, err := cli.ImageBuild(ctx, source, options)
	if err != nil {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return err
	}
	return logDockerResponse(common.Logger(ctx), response.Body, false)
}
func buildWithPlatformRecovery(ctx context.Context, cli client.APIClient, source io.Reader, options client.ImageBuildOptions, name string) error {
	replay, err := newReplayBuildContextLimitContext(ctx, source, maxBuildContextBytes)
	if err != nil {
		return err
	}
	defer replay.close()
	dockerfile, parseErr := replay.dockerfile(options.Dockerfile)
	if parseErr != nil {
		return fmt.Errorf("inspect Docker build context: %w", parseErr)
	}
	// Unresolved Dockerfile platform syntax never authorizes a guessed retry.
	expected := effectiveBuildPlatform{}
	if parseErr == nil && len(options.Platforms) == 1 {
		target := options.Platforms[0]
		if target.Variant == "" {
			target.Architecture, target.Variant, _ = strings.Cut(target.Architecture, "/")
		}
		expected = resolveFinalBuildPlatform(dockerfile, target)
	}
	body, err := replay.reader()
	if err != nil {
		return err
	}
	if err := executeClassicBuild(ctx, cli, body, options); err != nil {
		return err
	}
	if !expected.known {
		return nil
	}
	image, err := inspectBuiltImage(ctx, cli, name, expected.platform)
	if err != nil {
		return err
	}
	if buildPlatformMatches(image, expected.platform) {
		return nil
	}
	common.Logger(ctx).Warnf("Docker cached build returned %s/%s instead of %s/%s; retrying the identical classic context with parent pull", image.Os, image.Architecture, expected.platform.OS, expected.platform.Architecture)
	options.PullParent = true
	body, err = replay.reader()
	if err != nil {
		return err
	}
	if err := executeClassicBuild(ctx, cli, body, options); err != nil {
		return err
	}
	image, err = inspectBuiltImage(ctx, cli, name, expected.platform)
	if err != nil {
		return err
	}
	if !buildPlatformMatches(image, expected.platform) {
		return fmt.Errorf("Docker build platform mismatch after parent pull: got %s/%s, expected %s/%s", image.Os, image.Architecture, expected.platform.OS, expected.platform.Architecture)
	}
	return nil
}

// Custom blocking sources must implement Close to interrupt Read. Finite
// in-memory readers need no interrupt. Preserve caller ownership on success;
// only cancellation invokes the original source's Close.
type cancellableBuildInput struct {
	io.Reader
	source io.Closer
}

func (cancellableBuildInput) Close() error             { return nil }
func (source cancellableBuildInput) CancelRead() error { return source.source.Close() }
func interruptibleBuildInput(source io.Reader) (io.ReadCloser, error) {
	if closer, ok := source.(io.ReadCloser); ok {
		return cancellableBuildInput{Reader: closer, source: closer}, nil
	}
	switch source.(type) {
	case *bytes.Buffer, *bytes.Reader, *strings.Reader:
		return io.NopCloser(source), nil
	}
	return nil, errors.New("custom Docker build context must implement Close to interrupt a blocking Read")
}

type contextBuildReader struct {
	ctx    context.Context
	source io.Reader
}

func (source contextBuildReader) Read(data []byte) (int, error) {
	if err := source.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := source.source.Read(data)
	if source.ctx.Err() != nil {
		return count, source.ctx.Err()
	}
	return count, err
}
