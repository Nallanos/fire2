package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	authpkg "github/nallanos/fire2/internal/packages/auth"
)

// AuthHeader creates a user and a non-expiring session straight through the
// auth service and returns the Authorization header value that carries it
// ("Bearer <token>"): every sandbox route sits behind RequireAuth.
func AuthHeader(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()

	svc := authpkg.NewService(authpkg.NewPostgresRepository(pool))
	user, err := svc.EnsureUser(ctx, "test@fire.local", "test-password")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	session, err := svc.CreateNonExpiringSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return "Bearer " + session.Token
}
