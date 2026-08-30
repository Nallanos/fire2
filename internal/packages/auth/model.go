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
	ExpiresAt time.Time
}
