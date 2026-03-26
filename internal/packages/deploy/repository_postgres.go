package deploy

import (
	"context"
	"time"

	"railway_like/internal/db"
)

type PostgresRepository struct {
	q db.Querier
}

type Repository interface {
	Create(ctx context.Context, d Deployment) (Deployment, error)
	GetByID(ctx context.Context, id string) (Deployment, error)
	List(ctx context.Context) ([]Deployment, error)
	Update(ctx context.Context, d Deployment) (Deployment, error)
	GetByBuildID(ctx context.Context, buildID string) (Deployment, error)
}

func NewPostgresRepository(q db.Querier) *PostgresRepository {
	return &PostgresRepository{q: q}
}

func (r *PostgresRepository) Create(ctx context.Context, d Deployment) (*Deployment, error) {
	res, err := r.q.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:        d.ID,
		BuildID:   d.BuildID,
		Status:    string(d.Status),
		ImageTag:  d.ImageTag,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
		Port:      d.Port,
	})
	if err != nil {
		return nil, err
	}

	return &Deployment{
		ID:        res.ID,
		BuildID:   res.BuildID,
		Status:    DeploymentStatus(res.Status),
		ImageTag:  res.ImageTag,
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
		Port:      res.Port,
	}, nil
}

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (Deployment, error) {
	res, err := r.q.GetDeployment(ctx, id)
	if err != nil {
		return Deployment{}, err
	}

	return Deployment{
		ID:        res.ID,
		BuildID:   res.BuildID,
		Status:    DeploymentStatus(res.Status),
		ImageTag:  res.ImageTag,
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
		Port:      res.Port,
	}, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Deployment, error) {
	res, err := r.q.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}

	deployments := make([]Deployment, len(res))
	for i, d := range res {
		deployments[i] = Deployment{
			ID:        d.ID,
			BuildID:   d.BuildID,
			Status:    DeploymentStatus(d.Status),
			ImageTag:  d.ImageTag,
			CreatedAt: d.CreatedAt,
			UpdatedAt: d.UpdatedAt,
			Port:      d.Port,
		}
	}
	return deployments, nil
}

func (r *PostgresRepository) Update(ctx context.Context, d Deployment) (Deployment, error) {
	res, err := r.q.UpdateDeployment(ctx, db.UpdateDeploymentParams{
		ID:        d.ID,
		Status:    string(d.Status),
		UpdatedAt: time.Now().UTC(),
		Port:      d.Port,
	})
	if err != nil {
		return Deployment{}, err
	}

	return Deployment{
		ID:        res.ID,
		BuildID:   res.BuildID,
		Status:    DeploymentStatus(res.Status),
		ImageTag:  res.ImageTag,
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
		Port:      res.Port,
	}, nil
}

func (r *PostgresRepository) GetByBuildID(ctx context.Context, buildID string) (Deployment, error) {
	res, err := r.q.GetDeploymentByBuildID(ctx, buildID)
	if err != nil {
		return Deployment{}, err
	}

	return Deployment{
		ID:        res.ID,
		BuildID:   res.BuildID,
		Status:    DeploymentStatus(res.Status),
		ImageTag:  res.ImageTag,
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
		Port:      res.Port,
	}, nil
}
