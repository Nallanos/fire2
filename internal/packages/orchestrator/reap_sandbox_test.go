//go:build integration

package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

type reapEnv struct {
	pool        *pgxpool.Pool
	sandboxRepo sandboxpkg.Repository
	workerRepo  workerpkg.Repository
	fake        *testutil.FakeWorkerServer
	host        string
	port        int
	river       *river.Client[pgx.Tx]
}

// newReapEnv wires a database, a fake worker gRPC server and a River client
// running the reaper (with the given liveness timeout), cleanup and orphan workers.
func newReapEnv(t *testing.T, ctx context.Context, liveness time.Duration) *reapEnv {
	t.Helper()
	pool := testutil.SetupPostgres(t, ctx)
	e := &reapEnv{
		pool:        pool,
		sandboxRepo: sandboxpkg.NewPostgresRepository(pool),
		workerRepo:  workerpkg.NewPostgresRepository(pool),
		fake:        testutil.NewFakeWorkerServer(0, 0),
	}
	e.host, e.port = e.fake.StartServer(t)
	e.river = testutil.SetupRiverClient(t, ctx, pool, func(w *river.Workers) {
		river.AddWorker(w, NewReapSandboxWorker(e.sandboxRepo, pool, liveness))
		river.AddWorker(w, NewCleanupSandboxWorker(e.sandboxRepo, e.workerRepo))
		river.AddWorker(w, NewRemoveOrphanWorker(e.workerRepo))
	})
	return e
}

// registerWorker registers worker id pointing at the fake worker server and
// returns its fencing token.
func (e *reapEnv) registerWorker(t *testing.T, ctx context.Context, id string) int64 {
	t.Helper()
	w, err := e.workerRepo.Register(ctx, workerpkg.Worker{
		ID: id, Status: workerpkg.WorkerStatusActive, Address: e.host, Port: e.port, Capacity: 10,
		Budget: workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 8192},
	})
	if err != nil {
		t.Fatalf("register worker %s: %v", id, err)
	}
	return w.FencingToken
}

func (e *reapEnv) seed(t *testing.T, ctx context.Context, id string, st sandboxpkg.Status, ttl int64, age time.Duration, workerID string) {
	t.Helper()
	var wid *string
	if workerID != "" {
		wid = &workerID
	}
	_, err := e.sandboxRepo.Create(ctx, sandboxpkg.Sandbox{
		ID: id, Runtime: "node", Status: st, Image: "node:20-alpine", Port: 3000,
		TTL: ttl, CreatedAt: time.Now().UTC().Add(-age), WorkerID: wid,
	})
	if err != nil {
		t.Fatalf("seed sandbox %s: %v", id, err)
	}
}

func (e *reapEnv) setLastSeen(t *testing.T, ctx context.Context, id string, ago time.Duration) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, `UPDATE sandboxes SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1`, id, ago.Seconds()); err != nil {
		t.Fatalf("set last_seen_at %s: %v", id, err)
	}
}

// lastSeenAge returns how long ago the sandbox was last seen (a huge value if never).
func (e *reapEnv) lastSeenAge(t *testing.T, ctx context.Context, id string) time.Duration {
	t.Helper()
	var secs float64
	err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(EXTRACT(EPOCH FROM now() - last_seen_at), 1e9) FROM sandboxes WHERE id = $1`, id).Scan(&secs)
	if err != nil {
		t.Fatalf("read last_seen_at %s: %v", id, err)
	}
	return time.Duration(secs * float64(time.Second))
}

func (e *reapEnv) runReaper(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := e.river.Insert(ctx, ReapSandboxArgs{}, nil); err != nil {
		t.Fatalf("insert reap job: %v", err)
	}
}

func (e *reapEnv) server(opts ...EventServerOption) *EventGRPCServer {
	opts = append([]EventServerOption{WithTransactions(e.pool), WithRiverClient(e.river)}, opts...)
	return NewEventGRPCServer(e.sandboxRepo, NewEventRepository(e.pool), e.workerRepo, opts...)
}

func (e *reapEnv) heartbeat(srv *EventGRPCServer, workerID string, token int64, complete bool, sandboxes ...*orchestratorv1.SandboxInventoryItem) error {
	_, err := srv.ReportWorkerHeartbeat(context.Background(), &orchestratorv1.WorkerHeartbeat{
		WorkerId: workerID, Status: "active", Address: e.host, Port: int32(e.port), Capacity: 10,
		FencingToken: token, InventoryComplete: complete, Sandboxes: sandboxes,
	})
	return err
}

func item(id, state string) *orchestratorv1.SandboxInventoryItem {
	return &orchestratorv1.SandboxInventoryItem{SandboxId: id, State: state}
}

func assertStatus(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, want sandboxpkg.Status) sandboxpkg.Sandbox {
	t.Helper()
	sbx, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if sbx.Status != want {
		t.Fatalf("sandbox %s: want status %s, got %s", id, want, sbx.Status)
	}
	return sbx
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TTL is read in seconds from created_at; ttl <= 0 expires immediately; the
// claimed sandbox ends stopped with its worker link cleared.
func TestReapSandbox_TTLIsInSeconds(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	e.registerWorker(t, ctx, "wk-ttl")

	e.seed(t, ctx, "sbx-expired", sandboxpkg.StatusRunning, 5, 10*time.Second, "wk-ttl")
	e.seed(t, ctx, "sbx-fresh", sandboxpkg.StatusRunning, 3600, 0, "wk-ttl") // 3600ns under the old bug
	e.seed(t, ctx, "sbx-zero", sandboxpkg.StatusPending, 0, 0, "")
	e.seed(t, ctx, "sbx-done", sandboxpkg.StatusStopped, 1, time.Hour, "")

	e.runReaper(t, ctx)

	waitForStatus(t, ctx, e.sandboxRepo, "sbx-expired", sandboxpkg.StatusStopped, 10*time.Second)
	waitForStatus(t, ctx, e.sandboxRepo, "sbx-zero", sandboxpkg.StatusStopped, 10*time.Second)

	if sbx := assertStatus(t, ctx, e.sandboxRepo, "sbx-expired", sandboxpkg.StatusStopped); sbx.WorkerID != nil {
		t.Fatalf("expired sandbox kept worker link %q", *sbx.WorkerID)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-fresh", sandboxpkg.StatusRunning)
	if n := e.fake.RemoveCallCount("sbx-expired"); n != 1 {
		t.Fatalf("expected one remote removal for sbx-expired, got %d", n)
	}
	if n := e.fake.RemoveCallCount("sbx-fresh"); n != 0 {
		t.Fatalf("fresh sandbox must not be removed, got %d calls", n)
	}
}

// Two reaper runs never enqueue the same sandbox twice: the claim flips the
// status, so the second run finds nothing.
func TestReapSandbox_ClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	e.seed(t, ctx, "sbx-once", sandboxpkg.StatusRunning, 0, time.Second, "")

	first, err := e.sandboxRepo.ClaimTTLExpired(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: ids=%v err=%v", first, err)
	}
	second, err := e.sandboxRepo.ClaimTTLExpired(ctx)
	if err != nil || len(second) != 0 {
		t.Fatalf("second claim must find nothing: ids=%v err=%v", second, err)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-once", sandboxpkg.StatusCleanupPending)
}

// A worker that does not confirm the removal leaves the sandbox cleanup_pending
// with its worker; a later attempt that succeeds finishes in the job's target
// status; exhausting the attempts drops the link and fails the sandbox.
func TestCleanupSandbox_RemoteRemovalFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	e.registerWorker(t, ctx, "wk-retry")
	cleanup := NewCleanupSandboxWorker(e.sandboxRepo, e.workerRepo)

	job := func(id string, attempt int) *river.Job[CleanupSandboxArgs] {
		j := fakeCleanupJob(id)
		j.Attempt = attempt
		j.Args.TargetStatus = string(sandboxpkg.StatusStopped)
		return j
	}

	e.seed(t, ctx, "sbx-retry", sandboxpkg.StatusCleanupPending, 0, time.Minute, "wk-retry")
	e.fake.SetRemoveError(errors.New("vm busy"))

	if err := cleanup.Work(ctx, job("sbx-retry", 1)); err == nil {
		t.Fatal("expected an error so River retries")
	}
	sbx := assertStatus(t, ctx, e.sandboxRepo, "sbx-retry", sandboxpkg.StatusCleanupPending)
	if sbx.WorkerID == nil || *sbx.WorkerID != "wk-retry" {
		t.Fatalf("worker link must be kept while removal is unconfirmed, got %v", sbx.WorkerID)
	}

	e.fake.SetRemoveError(nil)
	if err := cleanup.Work(ctx, job("sbx-retry", 2)); err != nil {
		t.Fatalf("retry after worker recovered: %v", err)
	}
	if sbx := assertStatus(t, ctx, e.sandboxRepo, "sbx-retry", sandboxpkg.StatusStopped); sbx.WorkerID != nil {
		t.Fatalf("worker link must be cleared after confirmed removal, got %q", *sbx.WorkerID)
	}

	// Final attempt, still failing: give up on the worker, end failed.
	e.seed(t, ctx, "sbx-giveup", sandboxpkg.StatusCleanupPending, 0, time.Minute, "wk-retry")
	e.fake.SetRemoveError(errors.New("worker gone"))
	if err := cleanup.Work(ctx, job("sbx-giveup", 5)); err != nil {
		t.Fatalf("final attempt must settle the sandbox, got %v", err)
	}
	if sbx := assertStatus(t, ctx, e.sandboxRepo, "sbx-giveup", sandboxpkg.StatusFailed); sbx.WorkerID != nil {
		t.Fatalf("worker link must be dropped on the final attempt, got %q", *sbx.WorkerID)
	}
}

// An unknown-to-the-worker sandbox (gRPC NotFound) counts as already removed.
func TestCleanupSandbox_NotFoundOnWorkerCountsAsRemoved(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	e.registerWorker(t, ctx, "wk-nf")
	e.seed(t, ctx, "sbx-nf", sandboxpkg.StatusCleanupPending, 0, time.Minute, "wk-nf")
	e.fake.SetRemoveError(status.Error(codes.NotFound, "not running"))

	if err := NewCleanupSandboxWorker(e.sandboxRepo, e.workerRepo).Work(ctx, fakeCleanupJob("sbx-nf")); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-nf", sandboxpkg.StatusFailed)
}

// An empty complete inventory is not an absent one: it lists nothing, so it
// refreshes nothing (past the first inventory of a token), while an incomplete
// inventory is ignored whatever it lists.
func TestHeartbeatInventory_EmptyVersusAbsent(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, 15*time.Second)
	token := e.registerWorker(t, ctx, "wk-inv")
	srv := e.server()
	e.seed(t, ctx, "sbx-inv", sandboxpkg.StatusRunning, 3600, 0, "wk-inv")

	// First complete inventory of the token: liveness delay restarts from now.
	e.setLastSeen(t, ctx, "sbx-inv", time.Hour)
	if err := e.heartbeat(srv, "wk-inv", token, true); err != nil {
		t.Fatalf("first empty inventory: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-inv"); age > 5*time.Second {
		t.Fatalf("first inventory should restart the liveness delay, last seen %s ago", age)
	}

	// Later empty inventory: the sandbox is not listed, so it ages.
	e.setLastSeen(t, ctx, "sbx-inv", time.Minute)
	if err := e.heartbeat(srv, "wk-inv", token, true); err != nil {
		t.Fatalf("empty inventory: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-inv"); age < 30*time.Second {
		t.Fatalf("empty inventory must not refresh sandbox, last seen %s ago", age)
	}

	// Absent inventory (incomplete) listing the sandbox: ignored.
	if err := e.heartbeat(srv, "wk-inv", token, false, item("sbx-inv", "Running")); err != nil {
		t.Fatalf("incomplete inventory: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-inv"); age < 30*time.Second {
		t.Fatalf("incomplete inventory must not refresh sandbox, last seen %s ago", age)
	}

	// Complete inventory listing it: refreshed.
	if err := e.heartbeat(srv, "wk-inv", token, true, item("sbx-inv", "Running")); err != nil {
		t.Fatalf("listing inventory: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-inv"); age > 5*time.Second {
		t.Fatalf("listed sandbox must be refreshed, last seen %s ago", age)
	}
}

// A reported Running instance advances a starting sandbox.
func TestHeartbeatInventory_RunningAdvancesStarting(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	token := e.registerWorker(t, ctx, "wk-adv")
	e.seed(t, ctx, "sbx-adv", sandboxpkg.StatusStarting, 3600, 0, "wk-adv")

	if err := e.heartbeat(e.server(), "wk-adv", token, true, item("sbx-adv", "Running")); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-adv", sandboxpkg.StatusRunning)
}

// A new incarnation restarts the liveness delay once, and the previous
// incarnation can no longer touch sandboxes (heartbeat or event).
func TestHeartbeatInventory_TokenChangeAndStaleToken(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, 15*time.Second)
	srv := e.server()
	oldToken := e.registerWorker(t, ctx, "wk-tok")
	e.seed(t, ctx, "sbx-tok", sandboxpkg.StatusRunning, 3600, 0, "wk-tok")
	e.seed(t, ctx, "sbx-start", sandboxpkg.StatusStarting, 3600, 0, "wk-tok")

	if err := e.heartbeat(srv, "wk-tok", oldToken, true, item("sbx-tok", "Running")); err != nil {
		t.Fatalf("heartbeat old token: %v", err)
	}

	newToken := e.registerWorker(t, ctx, "wk-tok")
	if newToken <= oldToken {
		t.Fatalf("token must advance: old=%d new=%d", oldToken, newToken)
	}

	// The old incarnation is rejected and leaves no trace.
	e.setLastSeen(t, ctx, "sbx-tok", time.Minute)
	err := e.heartbeat(srv, "wk-tok", oldToken, true, item("sbx-tok", "Running"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale inventory heartbeat: want FailedPrecondition, got %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-tok"); age < 30*time.Second {
		t.Fatalf("stale heartbeat refreshed a sandbox (last seen %s ago)", age)
	}
	_, err = srv.IngestSandboxEvent(ctx, &orchestratorv1.SandboxEvent{
		SandboxId: "sbx-start", WorkerId: "wk-tok", State: "Running", FencingToken: oldToken,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale event: want FailedPrecondition, got %v", err)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-start", sandboxpkg.StatusStarting)

	// The new incarnation's first complete inventory restarts the delay…
	if err := e.heartbeat(srv, "wk-tok", newToken, true); err != nil {
		t.Fatalf("heartbeat new token: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-tok"); age > 5*time.Second {
		t.Fatalf("new token's first inventory should restart the delay, last seen %s ago", age)
	}
	// …only once.
	e.setLastSeen(t, ctx, "sbx-tok", time.Minute)
	if err := e.heartbeat(srv, "wk-tok", newToken, true); err != nil {
		t.Fatalf("second heartbeat new token: %v", err)
	}
	if age := e.lastSeenAge(t, ctx, "sbx-tok"); age < 30*time.Second {
		t.Fatalf("only the first inventory of a token restarts the delay, last seen %s ago", age)
	}

	// Events from the current incarnation apply.
	if _, err := srv.IngestSandboxEvent(ctx, &orchestratorv1.SandboxEvent{
		SandboxId: "sbx-start", WorkerId: "wk-tok", State: "Running", FencingToken: newToken,
	}); err != nil {
		t.Fatalf("current event: %v", err)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-start", sandboxpkg.StatusRunning)
}

// A sandbox its worker stops listing is failed once the liveness delay passes,
// and cleaned up on the worker; listed ones are left alone.
func TestReapSandbox_LivenessExpiry(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, 15*time.Second)
	e.registerWorker(t, ctx, "wk-live")
	e.seed(t, ctx, "sbx-lost", sandboxpkg.StatusRunning, 3600, 0, "wk-live")
	e.seed(t, ctx, "sbx-alive", sandboxpkg.StatusRunning, 3600, 0, "wk-live")
	e.setLastSeen(t, ctx, "sbx-lost", 20*time.Second)
	e.setLastSeen(t, ctx, "sbx-alive", 5*time.Second)

	e.runReaper(t, ctx)

	waitForStatus(t, ctx, e.sandboxRepo, "sbx-lost", sandboxpkg.StatusFailed, 10*time.Second)
	if sbx := assertStatus(t, ctx, e.sandboxRepo, "sbx-lost", sandboxpkg.StatusFailed); sbx.WorkerID != nil {
		t.Fatalf("failed sandbox kept worker link %q", *sbx.WorkerID)
	}
	if n := e.fake.RemoveCallCount("sbx-lost"); n != 1 {
		t.Fatalf("expected one remote removal for sbx-lost, got %d", n)
	}
	assertStatus(t, ctx, e.sandboxRepo, "sbx-alive", sandboxpkg.StatusRunning)
}

// Sandboxes a worker hosts but the database does not attribute to it are
// removed from the worker.
func TestHeartbeatInventory_OrphansAreRemoved(t *testing.T) {
	ctx := context.Background()
	e := newReapEnv(t, ctx, time.Hour)
	token := e.registerWorker(t, ctx, "wk-orph")
	e.seed(t, ctx, "sbx-mine", sandboxpkg.StatusRunning, 3600, 0, "wk-orph")
	e.seed(t, ctx, "sbx-finished", sandboxpkg.StatusFailed, 3600, 0, "")

	err := e.heartbeat(e.server(), "wk-orph", token, true,
		item("sbx-mine", "Running"), item("sbx-finished", "Running"), item("sbx-unknown", "Running"))
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	waitFor(t, "orphan removals", func() bool {
		return e.fake.RemoveCallCount("sbx-finished") >= 1 && e.fake.RemoveCallCount("sbx-unknown") >= 1
	})
	if n := e.fake.RemoveCallCount("sbx-mine"); n != 0 {
		t.Fatalf("hosted sandbox must not be removed, got %d calls", n)
	}
}
