package deploy

import (
	"context"
	"time"

	"github/nallanos/fire2/internal/packages/docker"
)

type Service struct {
	repo   Repository
	docker docker.ClientInterface
}

func NewService(repo Repository, docker docker.ClientInterface) *Service {
	return &Service{
		repo:   repo,
		docker: docker,
	}
}

// Deploys a new container based on the given image tag and associates it with the provided build ID.
func (s *Service) Deploy(ctx context.Context, buildID string, imageTag string) (Deployment, error) {

	if err := s.docker.InspectImage(ctx, imageTag); err != nil {
		if err := s.docker.PullImage(ctx, imageTag); err != nil {
			return Deployment{}, err
		}
	}

	deployment, err := s.repo.GetByBuildID(ctx, buildID)

	containerID, err := s.docker.CreateContainer(ctx, imageTag, deployment.Port)
	if err != nil {
		return Deployment{}, err
	}

	if err := s.docker.StartContainer(ctx, containerID); err != nil {
		return Deployment{}, err
	}

	d := Deployment{
		ID:        containerID,
		BuildID:   buildID,
		Status:    DeploymentStatusRunning,
		ImageTag:  imageTag,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	return s.repo.Create(ctx, d)
}

// Stops a running container associated with the given deployment ID and updates its status in the repository.
func (s *Service) Stop(ctx context.Context, id string) (Deployment, error) {
	d, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Deployment{}, err
	}

	if err := s.docker.StopContainer(ctx, d.ID); err != nil {
		return Deployment{}, err
	}

	d.Status = DeploymentStatusStopped
	return s.repo.Update(ctx, d)
}

// Deletes a container associated with the given deployment ID and removes its record from the repository.
func (s *Service) Remove(ctx context.Context, id string) error {
	d, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}

	return s.docker.RemoveContainer(ctx, d.ID)
}

// Gets a deployment by its ID from the repository.
func (s *Service) GetByID(ctx context.Context, id string) (Deployment, error) {
	return s.repo.GetByID(ctx, id)
}

// Lists all deployments in the repository.
func (s *Service) List(ctx context.Context) ([]Deployment, error) {
	return s.repo.List(ctx)
}
