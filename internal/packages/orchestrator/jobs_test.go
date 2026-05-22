package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github/nallanos/fire2/internal/db"
)

// stubQuerier satisfies db.Querier for unit tests. Set getSandboxFn /
// listWorkersFn to control behaviour; all other methods panic.
type stubQuerier struct {
	getSandboxFn  func(ctx context.Context, id string) (db.Sandbox, error)
	listWorkersFn func(ctx context.Context) ([]db.Worker, error)
}

func (s *stubQuerier) GetSandbox(ctx context.Context, id string) (db.Sandbox, error) {
	if s.getSandboxFn != nil {
		return s.getSandboxFn(ctx, id)
	}
	panic("stubQuerier: unexpected call to GetSandbox")
}

func (s *stubQuerier) ListWorkers(ctx context.Context) ([]db.Worker, error) {
	if s.listWorkersFn != nil {
		return s.listWorkersFn(ctx)
	}
	panic("stubQuerier: unexpected call to ListWorkers")
}

func (s *stubQuerier) CreateSandbox(_ context.Context, _ db.CreateSandboxParams) (db.Sandbox, error) {
	panic("stubQuerier: unexpected call to CreateSandbox")
}
func (s *stubQuerier) CreateSandboxEvent(_ context.Context, _ db.CreateSandboxEventParams) (db.SandboxEvent, error) {
	panic("stubQuerier: unexpected call to CreateSandboxEvent")
}
func (s *stubQuerier) CreateWorker(_ context.Context, _ db.CreateWorkerParams) (db.Worker, error) {
	panic("stubQuerier: unexpected call to CreateWorker")
}
func (s *stubQuerier) DeleteSandbox(_ context.Context, _ string) error {
	panic("stubQuerier: unexpected call to DeleteSandbox")
}
func (s *stubQuerier) DeleteWorker(_ context.Context, _ string) error {
	panic("stubQuerier: unexpected call to DeleteWorker")
}
func (s *stubQuerier) GetWorker(_ context.Context, _ string) (db.Worker, error) {
	panic("stubQuerier: unexpected call to GetWorker")
}
func (s *stubQuerier) ListSandboxes(_ context.Context) ([]db.Sandbox, error) {
	panic("stubQuerier: unexpected call to ListSandboxes")
}
func (s *stubQuerier) UpdateSandbox(_ context.Context, _ db.UpdateSandboxParams) (db.Sandbox, error) {
	panic("stubQuerier: unexpected call to UpdateSandbox")
}
func (s *stubQuerier) UpdateSandboxRunning(_ context.Context, _ db.UpdateSandboxRunningParams) (db.Sandbox, error) {
	panic("stubQuerier: unexpected call to UpdateSandboxRunning")
}
func (s *stubQuerier) UpdateWorker(_ context.Context, _ db.UpdateWorkerParams) (db.Worker, error) {
	panic("stubQuerier: unexpected call to UpdateWorker")
}

func makeJob(sandboxID string) *river.Job[CreateSandboxArgs] {
	return &river.Job[CreateSandboxArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   CreateSandboxArgs{SandboxID: sandboxID, Runtime: "node"},
	}
}

func TestCreateSandboxWorker_AbandonedSandboxReturnsJobCancel(t *testing.T) {
	querier := &stubQuerier{
		getSandboxFn: func(_ context.Context, _ string) (db.Sandbox, error) {
			return db.Sandbox{Status: "failed"}, nil
		},
	}
	worker := NewCreateSandboxWorker(querier)

	err := worker.Work(context.Background(), makeJob("sandbox-abandoned"))

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("expected *river.JobCancelError, got %T: %v", err, err)
	}
	if !errors.Is(err, errSandboxAbandoned) {
		t.Fatalf("expected errSandboxAbandoned wrapped in JobCancelError, got: %v", err)
	}
}

func TestCreateSandboxWorker_EmptyWorkerPoolReturnsRetriableError(t *testing.T) {
	querier := &stubQuerier{
		getSandboxFn: func(_ context.Context, _ string) (db.Sandbox, error) {
			return db.Sandbox{Status: "queued"}, nil
		},
		listWorkersFn: func(_ context.Context) ([]db.Worker, error) {
			return nil, nil // empty pool, no error
		},
	}
	worker := NewCreateSandboxWorker(querier)

	err := worker.Work(context.Background(), makeJob("sandbox-no-workers"))

	if err == nil {
		t.Fatal("expected an error for empty worker pool, got nil")
	}
	var cancelErr *river.JobCancelError
	if errors.As(err, &cancelErr) {
		t.Fatalf("expected a retriable error, got non-retriable JobCancelError: %v", err)
	}
}

func TestCreateSandboxWorker_ListWorkersDBErrorReturnsRetriableError(t *testing.T) {
	dbErr := errors.New("db timeout")
	querier := &stubQuerier{
		getSandboxFn: func(_ context.Context, _ string) (db.Sandbox, error) {
			return db.Sandbox{Status: "queued"}, nil
		},
		listWorkersFn: func(_ context.Context) ([]db.Worker, error) {
			return nil, dbErr
		},
	}
	worker := NewCreateSandboxWorker(querier)

	err := worker.Work(context.Background(), makeJob("sandbox-db-error"))

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var cancelErr *river.JobCancelError
	if errors.As(err, &cancelErr) {
		t.Fatalf("expected a retriable error, got non-retriable JobCancelError: %v", err)
	}
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected error to wrap the DB error, got: %v", err)
	}
}
