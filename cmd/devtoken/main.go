// Command devtoken issues a non-expiring session token for local dev
// tooling (make sandbox-* targets). It is never reachable over HTTP — the
// public signup/login endpoints always issue normally-expiring sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github/nallanos/fire2/internal/app"
	"github/nallanos/fire2/internal/packages/auth"
)

func main() {
	email := flag.String("email", "dev@fire.local", "email of the dev user to create/reuse")
	password := flag.String("password", "dev-local-only", "password of the dev user to create/reuse")
	flag.Parse()

	cfg := app.ConfigFromEnv()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	svc := auth.NewService(auth.NewPostgresRepository(pool))

	user, err := svc.EnsureUser(ctx, *email, *password)
	if err != nil {
		log.Fatalf("ensure dev user: %v", err)
	}

	session, err := svc.CreateNonExpiringSession(ctx, user.ID)
	if err != nil {
		log.Fatalf("create non-expiring session: %v", err)
	}

	fmt.Fprintln(os.Stdout, session.Token)
}
