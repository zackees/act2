package doctor

import (
	"context"
	"os"
	"time"

	"github.com/moby/moby/client"
	"github.com/nektos/act/pkg/serve"
)

// HostProbes reads the real host: Docker from the environment, files, and
// the serve socket.
func HostProbes(socket string) Probes {
	docker, dockerErr := client.New(client.FromEnv)
	bounded := func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 5*time.Second)
	}
	return Probes{
		Ping: func(ctx context.Context) error {
			if dockerErr != nil {
				return dockerErr
			}
			ctx, cancel := bounded(ctx)
			defer cancel()
			_, err := docker.Ping(ctx, client.PingOptions{})
			return err
		},
		Info: func(ctx context.Context) (DockerInfo, error) {
			ctx, cancel := bounded(ctx)
			defer cancel()
			result, err := docker.Info(ctx, client.InfoOptions{})
			if err != nil {
				return DockerInfo{}, err
			}
			info := DockerInfo{CgroupVersion: result.Info.CgroupVersion, MemTotal: result.Info.MemTotal, DockerRootDir: result.Info.DockerRootDir}
			for _, option := range result.Info.SecurityOptions {
				if option == "name=rootless" {
					info.Rootless = true
				}
			}
			return info, nil
		},
		ReadFile:  os.ReadFile,
		FreeBytes: freeBytes,
		Serve: func(ctx context.Context) (serve.Health, error) {
			if _, err := os.Stat(socket); err != nil {
				return serve.Health{}, err
			}
			ctx, cancel := bounded(ctx)
			defer cancel()
			return serve.NewClient(socket).Health(ctx)
		},
	}
}
