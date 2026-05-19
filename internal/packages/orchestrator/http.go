package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github/nallanos/fire2/internal/db"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

type HTTPHandlers struct {
	sandboxSvc  *sandboxpkg.Service
	db          db.Querier
	riverClient *river.Client[pgx.Tx]
}

func NewHTTPHandlers(sandboxSvc *sandboxpkg.Service, querier db.Querier, riverClient *river.Client[pgx.Tx]) *HTTPHandlers {
	return &HTTPHandlers{sandboxSvc: sandboxSvc, db: querier, riverClient: riverClient}
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

	image := body.Image
	if image == "" {
		image = defaultImageForRuntime(body.Runtime)
	}
	port := body.Port
	if port <= 0 {
		port = defaultSandboxPort()
	}

	// Pre-create sandbox at status=queued with resolved image and port so GET
	// endpoints return meaningful data while the job is in-flight.
	sbxRow, err := h.db.CreateSandbox(r.Context(), db.CreateSandboxParams{
		ID:         uuid.NewString(),
		Runtime:    body.Runtime,
		Status:     string(sandboxpkg.StatusQueued),
		Image:      image,
		Port:       port,
		Ttl:        body.TTL,
		PreviewUrl: body.PreviewURL,
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		log.Printf("pre-create sandbox failed: %v", err)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	// Subscribe before inserting the job to avoid a race where the job completes
	// before the select loop starts. River's subscription channel is buffered (1000)
	// so events are held even if we haven't read them yet.
	eventCh, cancelSub := h.riverClient.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer cancelSub()

	insertResult, err := h.riverClient.Insert(r.Context(), CreateSandboxArgs{
		SandboxID:  sbxRow.ID,
		Runtime:    body.Runtime,
		Image:      image,
		Port:       port,
		TTL:        body.TTL,
		PreviewURL: body.PreviewURL,
	}, nil)
	if err != nil {
		log.Printf("enqueue create_sandbox job failed: %v", err)
		_ = markSandboxFailed(r.Context(), h.db, sbxRow.ID)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	jobID := insertResult.Job.ID

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			log.Printf("create_sandbox timeout: sandbox=%s job=%d", sbxRow.ID, jobID)
			_ = markSandboxFailed(r.Context(), h.db, sbxRow.ID)
			http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
			return

		case event, ok := <-eventCh:
			if !ok {
				_ = markSandboxFailed(r.Context(), h.db, sbxRow.ID)
				http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
				return
			}
			if event.Job.ID != jobID {
				continue
			}
			switch event.Kind {
			case river.EventKindJobCompleted:
				finalSbx, fetchErr := h.sandboxSvc.GetByID(r.Context(), sbxRow.ID)
				if fetchErr != nil {
					log.Printf("fetch sandbox after job completed: %v", fetchErr)
					http.Error(w, sandboxpkg.ErrMsgFetchSandboxFailed, http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(finalSbx)
				return

			case river.EventKindJobFailed:
				// EventKindJobFailed fires for both retryable failures and final discard.
				// Only return 502 once all attempts are exhausted (JobStateDiscarded).
				if event.Job.State == rivertype.JobStateDiscarded {
					log.Printf("create_sandbox job exhausted: sandbox=%s job=%d", sbxRow.ID, jobID)
					_ = markSandboxFailed(r.Context(), h.db, sbxRow.ID)
					http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
					return
				}
				// JobStateRetryable: retry is scheduled, keep waiting.
			}
		}
	}
}

func markSandboxFailed(ctx context.Context, querier db.Querier, id string) error {
	_, err := querier.UpdateSandbox(ctx, db.UpdateSandboxParams{ID: id, Status: "failed"})
	return err
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
