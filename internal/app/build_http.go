package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github/nallanos/fire2/internal/packages/build"
)

type buildHTTPHandlers struct {
	svc *build.Service
}

func newBuildHTTPHandlers(svc *build.Service) *buildHTTPHandlers {
	return &buildHTTPHandlers{svc: svc}
}

func (h *buildHTTPHandlers) routes() http.Handler {
	r := chi.NewRouter()

	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.getByID)

	return r
}

type createBuildRequest struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

func (h *buildHTTPHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body createBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, build.ErrMsgInvalidJSON, http.StatusBadRequest)
		return
	}
	if body.Repo == "" {
		http.Error(w, build.ErrMsgRepoRequired, http.StatusBadRequest)
		return
	}
	if body.Ref == "" {
		body.Ref = "main"
	}

	b, err := h.svc.Create(r.Context(), build.CreateRequest{Repo: body.Repo, Ref: body.Ref})
	if err != nil {
		http.Error(w, build.ErrMsgCreateBuildFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(b)
}

func (h *buildHTTPHandlers) getByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, build.ErrMsgIDRequired, http.StatusBadRequest)
		return
	}

	b, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, build.ErrNotFound) {
			http.Error(w, build.ErrMsgNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, build.ErrMsgFetchBuildFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b)
}

func (h *buildHTTPHandlers) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		http.Error(w, build.ErrMsgListBuildsFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
