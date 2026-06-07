package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// fakeWorkerRepo is a minimal workerpkg.Repository for unit-testing the reaper
// without a database. Only MarkStaleInactive is exercised; the rest satisfy the
// interface.
type fakeWorkerRepo struct {
	gotTimeout time.Duration
	calls      int
	reaped     int64
	err        error
}

func (f *fakeWorkerRepo) Create(ctx context.Context, w workerpkg.Worker) (workerpkg.Worker, error) {
	return workerpkg.Worker{}, nil
}
func (f *fakeWorkerRepo) Get(ctx context.Context, id string) (workerpkg.Worker, error) {
	return workerpkg.Worker{}, nil
}
func (f *fakeWorkerRepo) Update(ctx context.Context, w workerpkg.Worker) (workerpkg.Worker, error) {
	return workerpkg.Worker{}, nil
}
func (f *fakeWorkerRepo) List(ctx context.Context) ([]workerpkg.Worker, error) {
	return nil, nil
}
func (f *fakeWorkerRepo) MarkStaleInactive(ctx context.Context, timeout time.Duration) (int64, error) {
	f.calls++
	f.gotTimeout = timeout
	return f.reaped, f.err
}

func TestReapWorkersWorker_PassesTimeoutAndSucceeds(t *testing.T) {
	repo := &fakeWorkerRepo{reaped: 3}
	w := NewReapWorkersWorker(repo, 15*time.Second)

	if err := w.Work(context.Background(), &river.Job[ReapWorkersArgs]{}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("expected MarkStaleInactive called once, got %d", repo.calls)
	}
	if repo.gotTimeout != 15*time.Second {
		t.Fatalf("expected timeout 15s passed through, got %s", repo.gotTimeout)
	}
}

func TestReapWorkersWorker_PropagatesError(t *testing.T) {
	repo := &fakeWorkerRepo{err: errors.New("boom")}
	w := NewReapWorkersWorker(repo, 15*time.Second)

	if err := w.Work(context.Background(), &river.Job[ReapWorkersArgs]{}); err == nil {
		t.Fatal("expected error from Work, got nil")
	}
}
