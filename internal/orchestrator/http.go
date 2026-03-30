package orchestrator

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

type HTTPHandlers struct {
	sandboxSvc *sandboxpkg.Service
	db         db.Querier
}

func NewHTTPHandlers(sandboxSvc *sandboxpkg.Service, db db.Querier) *HTTPHandlers {
	return &HTTPHandlers{sandboxSvc: sandboxSvc, db: db}
}

func (h *HTTPHandlers) Routes() http.Handler {
	r := chi.NewRouter()

	r.Post("/", h.createSandbox)
	r.Get("/", h.listSandboxes)
	r.Get("/{id}", h.getSandboxByID)

	return r
}

type createSandboxRequest struct {
	Runtime    string `json:"runtime"`
	Image      string `json:"image"`
	Port       int32  `json:"port"`
	TTL        int64  `json:"ttl"`
	PreviewURL string `json:"preview_url"`
}

func (h *HTTPHandlers) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, sandboxpkg.ErrMsgInvalidJSON, http.StatusBadRequest)
		return
	}
	if body.Runtime == "" {
		http.Error(w, sandboxpkg.ErrMsgRuntimeRequired, http.StatusBadRequest)
		return
	}
	if body.TTL <= 0 {
		body.TTL = 3600
	}

	workers, err := h.db.ListWorkers(r.Context())
	if err != nil {
		http.Error(w, "failed to list workers", http.StatusInternalServerError)
		return
	}
	if len(workers) == 0 {
		http.Error(w, "no workers available", http.StatusServiceUnavailable)
		return
	}

	image := body.Image
	if image == "" {
		image = defaultImageForRuntime(body.Runtime)
	}

	port := body.Port
	if port <= 0 {
		port = defaultSandboxPort()
	}

	grpcResp, err := CreateSandboxOnLeastUsedWorker(r.Context(), workers, &workerv1.CreateSandboxRequest{
		Id:         uuid.NewString(),
		Runtime:    body.Runtime,
		Image:      image,
		Port:       port,
		Ttl:        body.TTL,
		PreviewUrl: body.PreviewURL,
	})
	if err != nil {
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(grpcResp.GetSandbox())
}

func (h *HTTPHandlers) getSandboxByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, sandboxpkg.ErrMsgIDRequired, http.StatusBadRequest)
		return
	}

	sbx, err := h.sandboxSvc.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sandboxpkg.ErrNotFound) {
			http.Error(w, sandboxpkg.ErrMsgNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, sandboxpkg.ErrMsgFetchSandboxFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *HTTPHandlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	items, err := h.sandboxSvc.List(r.Context())
	if err != nil {
		http.Error(w, sandboxpkg.ErrMsgListSandboxesFailed, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func defaultImageForRuntime(runtime string) string {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "node", "nodejs", "javascript", "typescript":
		return "node:20-alpine"
	case "python", "py":
		return "python:3.12-alpine"
	case "go", "golang":
		return "golang:1.23-alpine"
	default:
		return "node:20-alpine"
	}
}

func defaultSandboxPort() int32 {
	return int32(10000 + (time.Now().UnixNano() % 50000))
}
