package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

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

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	orchestratorHandlers := orchestrator.NewHTTPHandlers(pool, sandboxRepo, workerRepo, riverClient)

	r.Route("/api", func(r chi.Router) {
		r.Mount("/sandboxes", orchestratorHandlers.Routes())
	})

	return &App{cfg: cfg, router: r}
}

func (a *App) Router() http.Handler {
	return a.router
}
