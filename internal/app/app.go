package app

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/build"
)

type App struct {
	cfg    Config
	router http.Handler
	db     *db.Queries
}

func New(cfg Config, sql *sql.DB) *App {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	db := db.New(sql)

	buildRepo := build.NewPostgresRepository(db)
	buildSvc := build.NewService(buildRepo)
	buildHandlers := newBuildHTTPHandlers(buildSvc)

	r.Route("/api", func(r chi.Router) {
		r.Mount("/builds", buildHandlers.routes())
	})

	return &App{cfg: cfg, router: r, db: db}
}

func (a *App) Router() http.Handler {
	return a.router
}
