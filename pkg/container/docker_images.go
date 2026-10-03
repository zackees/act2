//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/nektos/act/pkg/common"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, imageName string, platform string) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	inspectImage, err := cli.ImageInspect(ctx, imageName)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	imagePlatform := fmt.Sprintf("%s/%s", inspectImage.Os, inspectImage.Architecture)
	if inspectImage.Variant != "" && strings.Count(platform, "/") == 2 {
		imagePlatform += "/" + inspectImage.Variant
	}

	if platform == "" || platform == "any" || imagePlatform == platform {
		return true, nil
	}

	// A containerd store can hold multiple platforms behind the same tag.
	// The first inspect negotiated the API and returned its default variant.
	// Query the requested variant only when it differs, retaining the classic
	// comparison on older daemons that lack platform-aware inspection.
	if !versions.LessThan(cli.ClientVersion(), "1.49") {
		parts := strings.Split(platform, "/")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return false, fmt.Errorf("incorrect container platform option %q", platform)
		}
		requested := &specs.Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) == 3 {
			requested.Variant = parts[2]
		}
		selected, inspectErr := cli.ImageInspect(ctx, imageName, client.ImageInspectWithPlatform(requested))
		err = inspectErr
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return selected.Os == requested.OS && selected.Architecture == requested.Architecture &&
			(requested.Variant == "" || selected.Variant == requested.Variant), nil
	}

	logger := common.Logger(ctx)
	logger.Infof("image found but platform does not match: %s (image) != %s (platform)\n", imagePlatform, platform)

	return false, nil
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, imageName string, force bool, pruneChildren bool) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	inspectImage, err := cli.ImageInspect(ctx, imageName)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	if _, err = cli.ImageRemove(ctx, inspectImage.ID, client.ImageRemoveOptions{
		Force:         force,
		PruneChildren: pruneChildren,
	}); err != nil {
		return false, err
	}

	return true, nil
}
