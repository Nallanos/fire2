package auth

import "errors"

var (
	ErrNotFound        = errors.New("auth: not found")
	ErrEmailTaken      = errors.New("auth: email already registered")
	ErrInvalidCreds    = errors.New("auth: invalid email or password")
	ErrSessionExpired  = errors.New("auth: session expired")
)
