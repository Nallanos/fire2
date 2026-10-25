package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// getWorkerRepo is a worker repository that knows one worker.
type getWorkerRepo struct {
	fakeWorkerRepo
	worker workerpkg.Worker
}

func (r *getWorkerRepo) Get(context.Context, string) (workerpkg.Worker, error) {
	return r.worker, nil
}

// shortenCallTimeouts replaces the orchestrator-to-worker timeouts for one test.
func shortenCallTimeouts(t *testing.T, create, remove time.Duration) {
	t.Helper()
	prevCreate, prevRemove := createSandboxCallTimeout, removeSandboxCallTimeout
	createSandboxCallTimeout, removeSandboxCallTimeout = create, remove
	t.Cleanup(func() { createSandboxCallTimeout, removeSandboxCallTimeout = prevCreate, prevRemove })
}

// A worker the network drops packets to leaves RemoveSandbox unanswered. The
// removal gives up at its own deadline, with an error (so the cleanup job keeps
// the sandbox and River retries), instead of holding the job until River's
// timeout.
func TestRemoveSandboxOnWorkerGivesUpOnASilentWorker(t *testing.T) {
	shortenCallTimeouts(t, time.Minute, 300*time.Millisecond)

	fake := testutil.NewFakeWorkerServer(0, 0)
	fake.SetRemoveDelay(time.Minute)
	host, port := fake.StartServer(t)
	repo := &getWorkerRepo{worker: workerpkg.Worker{ID: "wk-silent", Address: host, Port: port}}

	began := time.Now()
	err := removeSandboxOnWorker(context.Background(), repo, "wk-silent", "sbx-silent")
	elapsed := time.Since(began)

	if err == nil {
		t.Fatal("a removal nobody answered was reported confirmed")
	}
	if !errors.Is(err, context.DeadlineExceeded) && status.Code(errors.Unwrap(err)) != codes.DeadlineExceeded {
		t.Errorf("removal error = %v, want a deadline exceeded", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("removal gave up after %s, want about the 300ms deadline", elapsed)
	}
	if fake.RemoveCallCount("sbx-silent") != 1 {
		t.Fatalf("RemoveSandbox calls = %d, want 1", fake.RemoveCallCount("sbx-silent"))
	}
}

// A worker that answers in time is unaffected.
func TestRemoveSandboxOnWorkerConfirmsWithinItsDeadline(t *testing.T) {
	shortenCallTimeouts(t, time.Minute, 5*time.Second)

	fake := testutil.NewFakeWorkerServer(0, 0)
	fake.SetRemoveDelay(50 * time.Millisecond)
	host, port := fake.StartServer(t)
	repo := &getWorkerRepo{worker: workerpkg.Worker{ID: "wk-slow", Address: host, Port: port}}

	if err := removeSandboxOnWorker(context.Background(), repo, "wk-slow", "sbx-slow"); err != nil {
		t.Fatalf("removal = %v, want confirmed", err)
	}
}

// The job timeout River applies must hold the slowest attempt of the create
// job: a scheduling step, then a CreateSandbox call that runs to its deadline.
// River's own default (1 minute) leaves only 10 seconds for the database round
// trips in between.
func TestJobTimeoutHoldsAFullCreateAttempt(t *testing.T) {
	const databaseRoom = 20 * time.Second
	if want := candidatesTimeout + defaultCreateSandboxCallTimeout + databaseRoom; JobTimeout < want {
		t.Fatalf("JobTimeout = %s, want at least %s (candidates %s + CreateSandbox %s + %s of room)",
			JobTimeout, want, candidatesTimeout, defaultCreateSandboxCallTimeout, databaseRoom)
	}
	if defaultRemoveSandboxCallTimeout >= JobTimeout {
		t.Fatalf("a removal (%s) must fit in the job timeout (%s)", defaultRemoveSandboxCallTimeout, JobTimeout)
	}
}
