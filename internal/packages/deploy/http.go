package deploy

import (
	"encoding/json"
	"net/http"

	"github/nallanos/fire2/internal/httputil"

	"github.com/go-chi/chi/v5"
)

type HTTPHandlers struct {
	svc *Service
	w   *Workflow
}

func NewHTTPHandlers(svc *Service, w *Workflow) *HTTPHandlers {
	return &HTTPHandlers{svc: svc, w: w}
}

func (h *HTTPHandlers) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.Deploy)
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	return r
}

type createDeploymentRequest struct {
	BuildID  string `json:"build_id"`
	ImageTag string `json:"image_tag"`
}

func (h *HTTPHandlers) Deploy(w http.ResponseWriter, r *http.Request) {
	var req createDeploymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.BadRequest(w, "invalid request body")
		return
	}

	// call the workflow to deploy the container
	if req.ImageTag == "" {
		err := h.w.Deploy(r.Context(), req.BuildID)
		if err != nil {
			httputil.InternalError(w, "failed to deploy with image")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *HTTPHandlers) List(w http.ResponseWriter, r *http.Request) {
	deployments, err := h.svc.List(r.Context())
	if err != nil {
		httputil.InternalError(w, "failed to list deployments")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(deployments)
}

func (h *HTTPHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		httputil.NotFound(w, "deployment not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}
