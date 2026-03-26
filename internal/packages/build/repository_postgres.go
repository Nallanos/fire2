package build

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"railway_like/internal/db"
)

type PostgresRepository struct {
	q db.Querier
}

type Repository interface {
	Create(ctx context.Context, b Build) (Build, error)
	GetByID(ctx context.Context, id string) (Build, error)
	List(ctx context.Context) ([]Build, error)
	Update(ctx context.Context, b Build) (Build, error)
}

func NewPostgresRepository(q db.Querier) *PostgresRepository {
	return &PostgresRepository{q: q}
}

func (r *PostgresRepository) Create(ctx context.Context, b Build) (Build, error) {
	res, err := r.q.CreateBuild(ctx, db.CreateBuildParams{
		ID:        b.ID,
		Repo:      b.Repo,
		Ref:       b.Ref,
		Status:    string(b.Status),
		CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt,
	})
	if err != nil {
		return Build{}, err
	}

	return Build{
		ID:        res.ID,
		Repo:      res.Repo,
		Ref:       res.Ref,
		Status:    Status(res.Status),
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
	}, nil
}

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (Build, error) {
	res, err := r.q.GetBuild(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Build{}, ErrNotFound
		}
		return Build{}, err
	}

	return Build{
		ID:        res.ID,
		Repo:      res.Repo,
		Ref:       res.Ref,
		Status:    Status(res.Status),
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
	}, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Build, error) {
	res, err := r.q.ListBuilds(ctx)
	if err != nil {
		return nil, err
	}

	builds := make([]Build, len(res))
	for i, b := range res {
		builds[i] = Build{
			ID:        b.ID,
			Repo:      b.Repo,
			Ref:       b.Ref,
			Status:    Status(b.Status),
			CreatedAt: b.CreatedAt,
			UpdatedAt: b.UpdatedAt,
		}
	}
	return builds, nil
}

func (r *PostgresRepository) Update(ctx context.Context, b Build) (Build, error) {
	res, err := r.q.UpdateBuild(ctx, db.UpdateBuildParams{
		ID:        b.ID,
		Status:    string(b.Status),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return Build{}, err
	}

	return Build{
		ID:        res.ID,
		Repo:      res.Repo,
		Ref:       res.Ref,
		Status:    Status(res.Status),
		CreatedAt: res.CreatedAt,
		UpdatedAt: res.UpdatedAt,
	}, nil
}
