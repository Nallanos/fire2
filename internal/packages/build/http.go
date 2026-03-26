package build

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type HTTPHandlers struct {
	svc *Service
}

func NewHTTPHandlers(svc *Service) *HTTPHandlers {
	return &HTTPHandlers{svc: svc}
}

func (h *HTTPHandlers) Routes() http.Handler {
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

func (h *HTTPHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body createBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if body.Ref == "" {
		body.Ref = "main"
	}

	b, err := h.svc.Create(r.Context(), CreateRequest{Repo: body.Repo, Ref: body.Ref})
	if err != nil {
		http.Error(w, "failed to create build", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(b)
}

func (h *HTTPHandlers) getByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	b, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to fetch build", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b)
}

func (h *HTTPHandlers) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		http.Error(w, "failed to list builds", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
