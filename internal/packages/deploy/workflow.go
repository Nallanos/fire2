package deploy

import (
	"context"
	"crypto"
	"encoding/hex"
	"railway_like/internal/modules/build"
)

type Workflow struct {
	svc  Service
	b    *build.Builder
	repo Repository
}

// Deploy deploys a new container based on the image tag associated with the given deployment ID. It first retrieves the deployment details from the repository, then uses the service to deploy the container, and finally updates the deployment status in the repository.
func (w *Workflow) Deploy(ctx context.Context, deploymentID string) error {
	deployment, err := w.repo.GetByID(ctx, deploymentID)
	if err != nil {
		return err
	}

	deployment, err = w.svc.Deploy(ctx, deployment.BuildID, deployment.ImageTag)
	if err != nil {
		return err
	}
	return nil
}

// BuildImage builds a Docker image for the given app name and Dockerfile path, and tags it with a unique image ID based on the app name.
func (w *Workflow) BuildImage(ctx context.Context, appName string, filePath string) error {
	hash := crypto.SHA256.New().Sum([]byte(appName))
	imageID := appName + hex.EncodeToString(hash)[:8]

	err := w.b.BuildImage(ctx, imageID, filePath)
	if err != nil {
		return err
	}
	return nil
}
