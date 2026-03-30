package sandbox

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

type createSandboxRequest struct {
	Runtime    string `json:"runtime"`
	TTL        int64  `json:"ttl"`
	PreviewURL string `json:"preview_url"`
}

func (h *HTTPHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Runtime == "" {
		http.Error(w, "runtime is required", http.StatusBadRequest)
		return
	}
	if body.TTL <= 0 {
		body.TTL = 3600
	}

	sbx, err := h.svc.Create(r.Context(), CreateRequest{Runtime: body.Runtime, TTL: body.TTL, PreviewURL: body.PreviewURL})
	if err != nil {
		http.Error(w, "failed to create sandbox", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *HTTPHandlers) getByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	sbx, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to fetch sandbox", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *HTTPHandlers) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		http.Error(w, "failed to list sandboxes", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
