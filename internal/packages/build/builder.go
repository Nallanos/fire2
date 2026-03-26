package build

import (
	"context"

	packClient "github.com/buildpacks/pack/pkg/client"
	"github.com/buildpacks/pack/pkg/logging"
	dockerClient "github.com/docker/docker/client"
)

type Builder struct {
	file   string
	docker *dockerClient.Client
	logger logging.Logger
}

func NewBuilder(sourcepath string) *Builder {
	return &Builder{file: sourcepath}
}

func (b *Builder) BuildImage(ctx context.Context, imageID string, filePath string) error {
	pack, err := packClient.NewClient(packClient.WithLogger(b.logger))
	if err != nil {
		return err
	}

	err = pack.Build(ctx, packClient.BuildOptions{
		AppPath: filePath,
		Image:   "app-" + imageID,
		Builder: "paketobuildpacks/builder-jammy-base",
	})
	if err != nil {
		return err
	}

	return nil
}
