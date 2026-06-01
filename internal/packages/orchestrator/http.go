package orchestrator

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

type HTTPHandlers struct {
	pool        *pgxpool.Pool
	sandboxRepo sandboxpkg.Repository
	workerRepo  workerpkg.Repository
	riverClient *river.Client[pgx.Tx]
}

func NewHTTPHandlers(
	pool *pgxpool.Pool,
	sandboxRepo sandboxpkg.Repository,
	workerRepo workerpkg.Repository,
	riverClient *river.Client[pgx.Tx],
) *HTTPHandlers {
	return &HTTPHandlers{
		pool:        pool,
		sandboxRepo: sandboxRepo,
		workerRepo:  workerRepo,
		riverClient: riverClient,
	}
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
	if h.riverClient == nil {
		log.Printf("river client is not configured")
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	// Fast-path: refuse early if no workers are registered.
	workers, err := h.workerRepo.List(r.Context())
	if err != nil {
		log.Printf("list workers failed: %v", err)
		http.Error(w, "failed to list workers", http.StatusInternalServerError)
		return
	}
	if len(workers) == 0 {
		log.Printf("no workers available for sandbox creation")
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

	// Subscribe before the insert so we don't miss the completion event.
	eventCh, cancelSub := h.riverClient.Subscribe(
		river.EventKindJobCompleted,
		river.EventKindJobFailed,
		river.EventKindJobCancelled,
	)
	defer cancelSub()

	// Atomically create the sandbox row and enqueue the river job.
	sandboxID := uuid.NewString()
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		log.Printf("begin tx failed: %v", err)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	_, err = h.sandboxRepo.WithTx(tx).Create(r.Context(), sandboxpkg.Sandbox{
		ID:         sandboxID,
		Runtime:    body.Runtime,
		Status:     sandboxpkg.StatusPending,
		Image:      image,
		Port:       port,
		TTL:        body.TTL,
		PreviewURL: body.PreviewURL,
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		log.Printf("create sandbox record failed: id=%s err=%v", sandboxID, err)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	insertRes, err := h.riverClient.InsertTx(r.Context(), tx, CreateSandboxArgs{SandboxID: sandboxID}, nil)
	if err != nil {
		log.Printf("insert river job failed: id=%s err=%v", sandboxID, err)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("commit tx failed: id=%s err=%v", sandboxID, err)
		http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusInternalServerError)
		return
	}

	jobID := insertRes.Job.ID

	for {
		select {
		case <-r.Context().Done():
			log.Printf("create sandbox request canceled: id=%s err=%v", sandboxID, r.Context().Err())
			return
		case event, ok := <-eventCh:
			if !ok {
				log.Printf("river subscription closed before job completion: id=%s job=%d", sandboxID, jobID)
				http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
				return
			}
			if event == nil || event.Job == nil || event.Job.ID != jobID {
				continue
			}

			switch event.Kind {
			case river.EventKindJobCompleted:
				sbx, fetchErr := h.sandboxRepo.GetByID(r.Context(), sandboxID)
				if fetchErr != nil {
					http.Error(w, sandboxpkg.ErrMsgFetchSandboxFailed, http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(sbx)
				return
			case river.EventKindJobCancelled:
				http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
				return
			case river.EventKindJobFailed:
				if event.Job.State != rivertype.JobStateDiscarded {
					continue
				}
				http.Error(w, sandboxpkg.ErrMsgCreateSandboxFailed, http.StatusBadGateway)
				return
			}
		}
	}
}

func (h *HTTPHandlers) getSandboxByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, sandboxpkg.ErrMsgIDRequired, http.StatusBadRequest)
		return
	}

	sbx, err := h.sandboxRepo.GetByID(r.Context(), id)
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
	items, err := h.sandboxRepo.List(r.Context())
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
