package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	authpkg "github/nallanos/fire2/internal/packages/auth"
	"github/nallanos/fire2/internal/packages/orchestrator"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

type App struct {
	cfg    Config
	router http.Handler
}

func New(cfg Config, pool *pgxpool.Pool, riverClient *river.Client[pgx.Tx]) *App {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// The front (Vercel, plus localhost in dev) is a different origin from
	// this API — auth uses a Bearer token, not a cookie, so there's no
	// credentialed-request case to worry about and no reason to restrict
	// origins beyond "browsers should be allowed to call this at all".
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	orchestratorHandlers := orchestrator.NewHTTPHandlers(pool, sandboxRepo, workerRepo, riverClient)

	authRepo := authpkg.NewPostgresRepository(pool)
	authSvc := authpkg.NewService(authRepo)
	authHandlers := orchestrator.NewAuthHandlers(authSvc)

	r.Route("/api", func(r chi.Router) {
		r.Mount("/auth", authHandlers.Routes())
		r.Route("/sandboxes", func(r chi.Router) {
			r.Use(orchestrator.RequireAuth(authSvc))
			r.Mount("/", orchestratorHandlers.Routes())
		})
	})

	return &App{cfg: cfg, router: r}
}

func (a *App) Router() http.Handler {
	return a.router
}
