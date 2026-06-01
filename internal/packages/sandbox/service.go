package sandbox

import (
	"context"
	"strings"

	"github.com/docker/docker/errdefs"

	"github/nallanos/fire2/internal/packages/docker"
)

type Service struct {
	repo   Repository
	docker docker.ClientInterface
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func NewRuntimeService(repo Repository, dockerClient docker.ClientInterface) *Service {
	return &Service{repo: repo, docker: dockerClient}
}

func (s *Service) GetByID(ctx context.Context, id string) (Sandbox, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]Sandbox, error) {
	return s.repo.List(ctx)
}

type RuntimeCreateRequest struct {
	ID         string
	Runtime    string
	Image      string
	Port       int32
	TTL        int64
	PreviewURL string
}

// CreateAndStart ensures the Docker container for the given sandbox exists and is running.
// It is idempotent: if a container with the sandbox ID label already exists, it is reused.
// It does NOT write to the sandboxes DB table — the orchestrator owns that row.
func (s *Service) CreateAndStart(ctx context.Context, req RuntimeCreateRequest) error {
	if s.docker == nil {
		return ErrDockerClientRequired
	}
	if err := s.ensureImage(ctx, req.Image); err != nil {
		return err
	}

	// Idempotency: reuse existing container if it was already created.
	containerID, err := s.docker.FindContainerBySandboxID(ctx, req.ID)
	if err != nil {
		return err
	}

	if containerID == "" {
		containerID, err = s.docker.CreateContainer(ctx, req.Image, portStr(req.Port), req.ID)
		if err != nil {
			return err
		}
	}

	if err := s.docker.StartContainer(ctx, containerID); err != nil {
		_ = s.docker.RemoveContainer(ctx, containerID)
		return err
	}

	return nil
}

func (s *Service) ensureImage(ctx context.Context, image string) error {
	if strings.TrimSpace(image) == "" {
		return nil
	}
	if err := s.docker.InspectImage(ctx, image); err != nil {
		if errdefs.IsNotFound(err) {
			return s.docker.PullImage(ctx, image)
		}
		return err
	}
	return nil
}

func (s *Service) Stop(ctx context.Context, containerID string) error {
	if s.docker == nil {
		return ErrDockerClientRequired
	}
	return s.docker.StopContainer(ctx, containerID)
}

func (s *Service) Remove(ctx context.Context, sandboxID string) error {
	return s.RemoveBySandboxID(ctx, sandboxID)
}

func (s *Service) RemoveBySandboxID(ctx context.Context, sandboxID string) error {
	if s.docker == nil {
		return ErrDockerClientRequired
	}

	containerID, err := s.docker.FindContainerBySandboxID(ctx, sandboxID)
	if err != nil {
		return err
	}

	if containerID != "" {
		if err := s.docker.StopContainer(ctx, containerID); err != nil {
			return err
		}
		if err := s.docker.RemoveContainer(ctx, containerID); err != nil {
			return err
		}
	}

	if s.repo != nil {
		return s.repo.Delete(ctx, sandboxID)
	}
	return nil
}

func portStr(port int32) string {
	if port <= 0 {
		return "3000"
	}
	s := ""
	n := int(port)
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
