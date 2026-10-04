//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
 "archive/tar"
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
 specs "github.com/opencontainers/image-spec/specs-go/v1"
 "github.com/nektos/act/pkg/common"
)

// Replay is bounded independently of archive expansion. Files remain private
// and are removed on every return; a limit breach is an error, never EOF.
const maxBuildContextBytes int64 = 8 << 30
const maxBuildDockerfileBytes int64 = 4 << 20

type replayBuildContext struct { file *os.File }
func newReplayBuildContext(source io.Reader) (*replayBuildContext, error) {
 return newReplayBuildContextLimit(source,maxBuildContextBytes)
}
func newReplayBuildContextLimit(source io.Reader, limit int64) (*replayBuildContext,error) {
 file, err := os.CreateTemp("", "act-build-context-*")
 if err != nil { return nil, err }
 replay := &replayBuildContext{file:file}
 count, err := io.Copy(file, io.LimitReader(source, limit+1))
 if err == nil && count > limit { err = fmt.Errorf("Docker build context exceeds %d byte replay limit", limit) }
 if err != nil { replay.close(); return nil, err }
 return replay, nil
}
func (replay *replayBuildContext) close() {
 name := replay.file.Name()
 _ = replay.file.Close()
 _ = os.Remove(name)
}
func (replay *replayBuildContext) reader() (io.Reader, error) {
 _, err := replay.file.Seek(0, io.SeekStart)
 return replay.file, err
}
func (replay *replayBuildContext) dockerfile(name string) ([]byte, error) {
 source, err := replay.reader()
 if err != nil { return nil, err }
 decoded, err := compression.DecompressStream(source)
 if err != nil { return nil, err }
 defer decoded.Close()
 guard := &buildArchiveReader{source:decoded,limits:buildArchiveLimits{bytes:maxBuildContextBytes,headers:1000000,metadataBytes:1<<20,metadataTotal:8<<20}}
 reader := tar.NewReader(guard)
 var contents []byte
 found := false
 for {
  header, err := reader.Next()
  if errors.Is(err, io.EOF) { return contents, guard.finish() }
  if err != nil { return nil, err }
  if header.Size != guard.headerSize { return nil, errors.New("Docker build archive metadata changes raw entry size") }
  if path.Clean(header.Name) != path.Clean(strings.ReplaceAll(name, "\\", "/")) { continue }
  if found || header.Typeflag != tar.TypeReg || header.Size > maxBuildDockerfileBytes { return nil, errors.New("Dockerfile must be one regular file within the 4 MiB inspection limit") }
  found = true
  contents, err = io.ReadAll(reader)
  if err != nil { return nil, err }
 }
}
func buildPlatformMatches(image client.ImageInspectResult, expected specs.Platform) bool {
 return image.Os == expected.OS && image.Architecture == expected.Architecture && (expected.Variant == "" || image.Variant == expected.Variant)
}
func inspectBuiltImage(ctx context.Context, cli client.APIClient, name string, expected specs.Platform) (client.ImageInspectResult, error) {
 image, err := cli.ImageInspect(ctx, name)
 if err != nil || versions.LessThan(cli.ClientVersion(), "1.49") { return image, err }
 return cli.ImageInspect(ctx, name, client.ImageInspectWithPlatform(&expected))
}
func executeClassicBuild(ctx context.Context, cli client.APIClient, source io.Reader, options client.ImageBuildOptions) error {
 response, err := cli.ImageBuild(ctx, source, options)
 if err != nil {
  if response.Body != nil { _ = response.Body.Close() }
  return err
 }
 return logDockerResponse(common.Logger(ctx), response.Body, false)
}
func buildWithPlatformRecovery(ctx context.Context, cli client.APIClient, source io.Reader, options client.ImageBuildOptions, name string) error {
 replay, err := newReplayBuildContext(source)
 if err != nil { return err }
 defer replay.close()
 dockerfile, parseErr := replay.dockerfile(options.Dockerfile)
 // Docker remains authoritative for invalid/unsupported inputs. Such inputs
 // never authorize a forced-network retry from a guessed expected platform.
 expected := effectiveBuildPlatform{}
 if parseErr == nil && len(options.Platforms) == 1 {
  expected = resolveFinalBuildPlatform(dockerfile, options.Platforms[0])
 }
 body, err := replay.reader()
 if err != nil { return err }
 if err := executeClassicBuild(ctx, cli, body, options); err != nil { return err }
 if !expected.known { return nil }
 image, err := inspectBuiltImage(ctx, cli, name, expected.platform)
 if err != nil { return err }
 if buildPlatformMatches(image, expected.platform) { return nil }
 common.Logger(ctx).Warnf("Docker cached build returned %s/%s instead of %s/%s; retrying the identical classic context with parent pull", image.Os, image.Architecture, expected.platform.OS, expected.platform.Architecture)
 options.PullParent = true
 body, err = replay.reader()
 if err != nil { return err }
 if err := executeClassicBuild(ctx, cli, body, options); err != nil { return err }
 image, err = inspectBuiltImage(ctx, cli, name, expected.platform)
 if err != nil { return err }
 if !buildPlatformMatches(image, expected.platform) { return fmt.Errorf("Docker build platform mismatch after parent pull: got %s/%s, expected %s/%s", image.Os,image.Architecture,expected.platform.OS,expected.platform.Architecture) }
 return nil
}
