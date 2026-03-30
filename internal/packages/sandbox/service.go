package sandbox

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github/nallanos/fire2/internal/packages/docker"
)

type CreateRequest struct {
	Runtime    string
	TTL        int64
	PreviewURL string
}

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

func (s *Service) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	sandbox := Sandbox{
		ID:         uuid.NewString(),
		Runtime:    req.Runtime,
		Status:     StatusQueued,
		TTL:        req.TTL,
		PreviewURL: req.PreviewURL,
	}
	return s.repo.Create(ctx, sandbox)
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

func (s *Service) CreateAndStart(ctx context.Context, req RuntimeCreateRequest) (Sandbox, error) {
	if s.docker == nil {
		return Sandbox{}, ErrDockerClientRequired
	}

	containerID, err := s.docker.CreateContainer(ctx, req.Image, strconv.Itoa(int(req.Port)), req.ID)
	if err != nil {
		return Sandbox{}, err
	}

	if err := s.docker.StartContainer(ctx, containerID); err != nil {
		_ = s.docker.RemoveContainer(ctx, containerID)
		return Sandbox{}, err
	}

	sbx, err := s.repo.Create(ctx, Sandbox{
		ID:         req.ID,
		Runtime:    req.Runtime,
		Status:     StatusRunning,
		TTL:        req.TTL,
		CreatedAt:  time.Now().UTC(),
		Port:       req.Port,
		PreviewURL: req.PreviewURL,
		Image:      req.Image,
	})
	if err != nil {
		_ = s.docker.StopContainer(ctx, containerID)
		_ = s.docker.RemoveContainer(ctx, containerID)
		return Sandbox{}, err
	}

	return sbx, nil
}

func (s *Service) Stop(ctx context.Context, containerID string) error {
	if s.docker == nil {
		return ErrDockerClientRequired
	}

	return s.docker.StopContainer(ctx, containerID)
}

func (s *Service) Remove(ctx context.Context, containerID string) error {
	if s.docker == nil {
		return ErrDockerClientRequired
	}

	if err := s.docker.StopContainer(ctx, containerID); err != nil {
		return err
	}

	if err := s.docker.RemoveContainer(ctx, containerID); err != nil {
		return err
	}

	return s.repo.Delete(ctx, containerID)
}
