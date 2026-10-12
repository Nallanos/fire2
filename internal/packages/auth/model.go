package auth

import "time"

type User struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

type Session struct {
	Token     string
	UserID    string
	CreatedAt time.Time
	// ExpiresAt is nil for a session that never expires. Only internal
	// tooling issues those (see Service.CreateNonExpiringSession) — the
	// public signup/login endpoints always set it.
	ExpiresAt *time.Time
}
