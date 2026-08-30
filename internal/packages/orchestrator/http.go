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

	workerv1 "github/nallanos/fire2/gen/worker/v1"
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
	r.Delete("/{id}", h.deleteSandbox)
	return r
}

type createSandboxRequest struct {
	Runtime    string `json:"runtime"`
	Image      string `json:"image"`
	Port       int32  `json:"port"`
	TTL        int64  `json:"ttl"`
	PreviewURL string `json:"preview_url"`
	VcpuCount  int32  `json:"vcpu_count"`
	MemSizeMib int32  `json:"mem_size_mib"`
}

// Defaults match the Firecracker config validated manually during
// development. Applied whenever the caller doesn't specify a value, so
// requests from clients unaware of these fields still get a Firecracker-valid
// machine-config instead of a rejected vcpu_count of 0.
const (
	defaultVcpuCount  int32 = 1
	defaultMemSizeMib int32 = 256
)

func (h *HTTPHandlers) createSandbox(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

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
	vcpuCount := body.VcpuCount
	if vcpuCount <= 0 {
		vcpuCount = defaultVcpuCount
	}
	memSizeMib := body.MemSizeMib
	if memSizeMib <= 0 {
		memSizeMib = defaultMemSizeMib
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
		VcpuCount:  vcpuCount,
		MemSizeMib: memSizeMib,
		UserID:     &user.ID,
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
	user, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

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

	// 404 rather than 403 for someone else's sandbox — don't confirm the ID
	// exists to a caller who doesn't own it.
	if sbx.UserID == nil || *sbx.UserID != user.ID {
		http.Error(w, sandboxpkg.ErrMsgNotFound, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sbx)
}

func (h *HTTPHandlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	// List all workers in the database
	workers, err := h.workerRepo.List(r.Context())
	if err != nil {
		log.Printf("list workers failed: %v", err)
		http.Error(w, "failed to list workers", http.StatusInternalServerError)
		return
	}

	// Collect running sandboxes from all workers
	sandboxes := make([]sandboxpkg.Sandbox, 0)

	for _, worker := range workers {

		client, err := NewClient(r.Context(), normalizeWorkerAddress(worker.Address, int32(worker.Port)))
		if err != nil {
			log.Printf("failed to create gRPC client for worker %s: %v", worker.ID, err)
			continue
		}
		defer client.Close()

		sbxResp, err := client.ListRunningSandboxes(r.Context(), &workerv1.ListRunningSandboxesRequest{})
		if err != nil {
			log.Printf("failed to list running sandboxes for worker %s: %v", worker.ID, err)
			continue
		}

		for _, sbx := range sbxResp.ContainerIds {
			sandbox, err := h.sandboxRepo.GetByID(r.Context(), sbx)
			if err != nil {
				log.Printf("failed to fetch sandbox details for ID %s: %v", sbx, err)
				continue
			}
			if sandbox.UserID == nil || *sandbox.UserID != user.ID {
				continue
			}
			sandboxes = append(sandboxes, sandbox)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sandboxes)
}

// deleteSandbox asks for a sandbox to be torn down. Deletion is asynchronous
// — this only transitions the row to cleanup_pending and enqueues the same
// cleanup_sandbox job the create-job's exhausted-retry path uses, with
// TargetStatus set to "stopped" so a deliberate delete reads differently
// from a genuine failure once cleanup completes.
func (h *HTTPHandlers) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

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
	if sbx.UserID == nil || *sbx.UserID != user.ID {
		http.Error(w, sandboxpkg.ErrMsgNotFound, http.StatusNotFound)
		return
	}

	// Already terminal (or already being cleaned up): idempotent no-op.
	switch sbx.Status {
	case sandboxpkg.StatusStopped, sandboxpkg.StatusFailed, sandboxpkg.StatusCleanupPending, sandboxpkg.StatusCleanedUp:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if h.riverClient == nil {
		log.Printf("river client is not configured")
		http.Error(w, "delete sandbox failed", http.StatusInternalServerError)
		return
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		log.Printf("delete sandbox: begin tx failed: id=%s err=%v", id, err)
		http.Error(w, "delete sandbox failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	_, n, err := h.sandboxRepo.WithTx(tx).UpdateStatus(r.Context(), id, sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusPending, sandboxpkg.StatusScheduling, sandboxpkg.StatusAssigned,
		sandboxpkg.StatusStarting, sandboxpkg.StatusRunning)
	if err != nil {
		log.Printf("delete sandbox: set cleanup_pending failed: id=%s err=%v", id, err)
		http.Error(w, "delete sandbox failed", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		// Status advanced concurrently (e.g. it just failed on its own) —
		// idempotent from the caller's point of view.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := h.riverClient.InsertTx(r.Context(), tx, CleanupSandboxArgs{
		SandboxID:    id,
		TargetStatus: string(sandboxpkg.StatusStopped),
	}, &river.InsertOpts{Queue: "cleanup"}); err != nil {
		log.Printf("delete sandbox: insert cleanup job failed: id=%s err=%v", id, err)
		http.Error(w, "delete sandbox failed", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("delete sandbox: commit tx failed: id=%s err=%v", id, err)
		http.Error(w, "delete sandbox failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
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
