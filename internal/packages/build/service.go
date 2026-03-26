package build

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type CreateRequest struct {
	Repo string
	Ref  string
}

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (Build, error) {
	now := time.Now().UTC()
	b := Build{
		ID:        uuid.NewString(),
		Repo:      req.Repo,
		Ref:       req.Ref,
		Status:    StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return s.repo.Create(ctx, b)
}

func (s *Service) GetByID(ctx context.Context, id string) (Build, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]Build, error) {
	return s.repo.List(ctx)
}
