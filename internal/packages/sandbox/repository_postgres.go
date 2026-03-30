package sandbox

import (
	"context"
	"database/sql"
	"errors"

	"github/nallanos/fire2/internal/db"
)

type PostgresRepository struct {
	q db.Querier
}

type Repository interface {
	Create(ctx context.Context, s Sandbox) (Sandbox, error)
	Delete(ctx context.Context, id string) error
	GetByID(ctx context.Context, id string) (Sandbox, error)
	List(ctx context.Context) ([]Sandbox, error)
	Update(ctx context.Context, s Sandbox) (Sandbox, error)
}

func NewPostgresRepository(q db.Querier) *PostgresRepository {
	return &PostgresRepository{q: q}
}

func (r *PostgresRepository) Create(ctx context.Context, s Sandbox) (Sandbox, error) {
	res, err := r.q.CreateSandbox(ctx, db.CreateSandboxParams{
		ID:         s.ID,
		Runtime:    s.Runtime,
		Status:     string(s.Status),
		Image:      s.Image,
		Port:       s.Port,
		Ttl:        s.TTL,
		PreviewUrl: s.PreviewURL,
		CreatedAt:  s.CreatedAt,
	})
	if err != nil {
		return Sandbox{}, err
	}

	return Sandbox{
		ID:         res.ID,
		Runtime:    res.Runtime,
		Status:     Status(res.Status),
		TTL:        res.Ttl,
		CreatedAt:  res.CreatedAt,
		Port:       res.Port,
		PreviewURL: res.PreviewUrl,
		Image:      res.Image,
	}, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, id string) error {
	return r.q.DeleteSandbox(ctx, id)
}

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (Sandbox, error) {
	res, err := r.q.GetSandbox(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Sandbox{}, ErrNotFound
		}
		return Sandbox{}, err
	}

	return Sandbox{
		ID:         res.ID,
		Runtime:    res.Runtime,
		Status:     Status(res.Status),
		TTL:        res.Ttl,
		CreatedAt:  res.CreatedAt,
		Port:       res.Port,
		PreviewURL: res.PreviewUrl,
		Image:      res.Image,
	}, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Sandbox, error) {
	res, err := r.q.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}

	sandboxes := make([]Sandbox, len(res))
	for i, s := range res {
		sandboxes[i] = Sandbox{
			ID:         s.ID,
			Runtime:    s.Runtime,
			Status:     Status(s.Status),
			TTL:        s.Ttl,
			CreatedAt:  s.CreatedAt,
			Port:       s.Port,
			PreviewURL: s.PreviewUrl,
			Image:      s.Image,
		}
	}
	return sandboxes, nil
}

func (r *PostgresRepository) Update(ctx context.Context, s Sandbox) (Sandbox, error) {
	res, err := r.q.UpdateSandbox(ctx, db.UpdateSandboxParams{
		ID:     s.ID,
		Status: string(s.Status),
	})
	if err != nil {
		return Sandbox{}, err
	}

	return Sandbox{
		ID:         res.ID,
		Runtime:    res.Runtime,
		Status:     Status(res.Status),
		TTL:        res.Ttl,
		CreatedAt:  res.CreatedAt,
		Port:       res.Port,
		PreviewURL: res.PreviewUrl,
		Image:      res.Image,
	}, nil
}
