package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

// Repository is the storage interface for users and sessions.
type Repository interface {
	CreateUser(ctx context.Context, u User) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	GetUserByID(ctx context.Context, id string) (User, error)
	CreateSession(ctx context.Context, s Session) (Session, error)
	GetSession(ctx context.Context, token string) (Session, error)
	DeleteSession(ctx context.Context, token string) error
}

type PostgresRepository struct {
	db pgxdb.DBTX
}

func NewPostgresRepository(db pgxdb.DBTX) *PostgresRepository {
	return &PostgresRepository{db: db}
}

func (r *PostgresRepository) CreateUser(ctx context.Context, u User) (User, error) {
	const q = `
		INSERT INTO users (id, email, password_hash, created_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, password_hash, created_at`

	row := r.db.QueryRow(ctx, q, u.ID, u.Email, u.PasswordHash, u.CreatedAt)
	created, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	return created, nil
}

func (r *PostgresRepository) GetUserByEmail(ctx context.Context, email string) (User, error) {
	const q = `SELECT id, email, password_hash, created_at FROM users WHERE email = $1`
	return r.getUser(ctx, q, email)
}

func (r *PostgresRepository) GetUserByID(ctx context.Context, id string) (User, error) {
	const q = `SELECT id, email, password_hash, created_at FROM users WHERE id = $1`
	return r.getUser(ctx, q, id)
}

func (r *PostgresRepository) getUser(ctx context.Context, q string, arg string) (User, error) {
	row := r.db.QueryRow(ctx, q, arg)
	u, err := scanUser(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return u, nil
}

func (r *PostgresRepository) CreateSession(ctx context.Context, s Session) (Session, error) {
	const q = `
		INSERT INTO sessions (token, user_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING token, user_id, created_at, expires_at`

	row := r.db.QueryRow(ctx, q, s.Token, s.UserID, s.CreatedAt, s.ExpiresAt)
	return scanSession(row)
}

func (r *PostgresRepository) GetSession(ctx context.Context, token string) (Session, error) {
	const q = `SELECT token, user_id, created_at, expires_at FROM sessions WHERE token = $1`
	row := r.db.QueryRow(ctx, q, token)
	s, err := scanSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	return s, nil
}

func (r *PostgresRepository) DeleteSession(ctx context.Context, token string) error {
	const q = `DELETE FROM sessions WHERE token = $1`
	_, err := r.db.Exec(ctx, q, token)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt); err != nil {
		return User{}, err
	}
	return u, nil
}

func scanSession(row scanner) (Session, error) {
	var s Session
	if err := row.Scan(&s.Token, &s.UserID, &s.CreatedAt, &s.ExpiresAt); err != nil {
		return Session{}, err
	}
	return s, nil
}
