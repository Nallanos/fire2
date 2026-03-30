package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	sandbox "github/nallanos/fire2/internal/packages/sandbox"
)

type sandboxHTTPHandlers struct {
	svc *sandbox.Service
}

func newSandboxHTTPHandlers(svc *sandbox.Service) *sandboxHTTPHandlers {
	return &sandboxHTTPHandlers{svc: svc}
}

func (h *sandboxHTTPHandlers) routes() http.Handler {
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

func (h *sandboxHTTPHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, sandbox.ErrMsgInvalidJSON, http.StatusBadRequest)
		return
	}
	if body.Runtime == "" {
		http.Error(w, sandbox.ErrMsgRuntimeRequired, http.StatusBadRequest)
		return
	}
	if body.TTL <= 0 {
		body.TTL = 3600
	}

	sbx, err := h.svc.Create(r.Context(), sandbox.CreateRequest{Runtime: body.Runtime, TTL: body.TTL, PreviewURL: body.PreviewURL})
	if err != nil {
		http.Error(w, sandbox.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *sandboxHTTPHandlers) getByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, sandbox.ErrMsgIDRequired, http.StatusBadRequest)
		return
	}

	sbx, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			http.Error(w, sandbox.ErrMsgNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, sandbox.ErrMsgFetchSandboxFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *sandboxHTTPHandlers) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		http.Error(w, sandbox.ErrMsgListSandboxesFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
