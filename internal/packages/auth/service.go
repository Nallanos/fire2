package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const sessionTTL = 30 * 24 * time.Hour

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// SignUp creates a new user and an initial session for them.
func (s *Service) SignUp(ctx context.Context, email, password string) (User, Session, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, Session{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.repo.CreateUser(ctx, User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		return User{}, Session{}, err
	}

	session, err := s.createSession(ctx, user.ID)
	if err != nil {
		return User{}, Session{}, err
	}
	return user, session, nil
}

// Login validates credentials and issues a new session.
func (s *Service) Login(ctx context.Context, email, password string) (User, Session, error) {
	user, err := s.repo.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return User{}, Session{}, ErrInvalidCreds
		}
		return User{}, Session{}, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return User{}, Session{}, ErrInvalidCreds
	}

	session, err := s.createSession(ctx, user.ID)
	if err != nil {
		return User{}, Session{}, err
	}
	return user, session, nil
}

// Authenticate resolves a session token to its owning user. Returns
// ErrSessionExpired for an expired-but-existing token so callers can tell
// "never logged in" from "logged in, needs to again" apart. A nil
// ExpiresAt (see CreateNonExpiringSession) never expires.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	session, err := s.repo.GetSession(ctx, token)
	if err != nil {
		return User{}, err
	}
	if session.ExpiresAt != nil && time.Now().After(*session.ExpiresAt) {
		return User{}, ErrSessionExpired
	}
	return s.repo.GetUserByID(ctx, session.UserID)
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.repo.DeleteSession(ctx, token)
}

// EnsureUser finds the user with the given email, creating it if absent.
// Intended for internal tooling (cmd/devtoken), not the public signup flow —
// it never errors on an existing email.
func (s *Service) EnsureUser(ctx context.Context, email, password string) (User, error) {
	user, err := s.repo.GetUserByEmail(ctx, email)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.repo.CreateUser(ctx, User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().UTC(),
	})
}

// CreateNonExpiringSession issues a session with no expiry. Only for
// internal tooling — never reachable through the public API, which always
// goes through createSession and gets the normal TTL.
func (s *Service) CreateNonExpiringSession(ctx context.Context, userID string) (Session, error) {
	return s.createSessionWithTTL(ctx, userID, nil)
}

func (s *Service) createSession(ctx context.Context, userID string) (Session, error) {
	ttl := sessionTTL
	return s.createSessionWithTTL(ctx, userID, &ttl)
}

// createSessionWithTTL issues a session expiring after ttl, or never if ttl
// is nil.
func (s *Service) createSessionWithTTL(ctx context.Context, userID string, ttl *time.Duration) (Session, error) {
	token, err := randomToken()
	if err != nil {
		return Session{}, fmt.Errorf("generate session token: %w", err)
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if ttl != nil {
		t := now.Add(*ttl)
		expiresAt = &t
	}
	return s.repo.CreateSession(ctx, Session{
		Token:     token,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
}

// randomToken returns a 256-bit random value, hex-encoded — enough entropy
// that guessing a valid session token is infeasible.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
