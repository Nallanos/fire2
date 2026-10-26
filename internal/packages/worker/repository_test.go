//go:build integration

package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github/nallanos/fire2/internal/testutil"
)

func TestWorkerRepository_FencingToken(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	w := Worker{
		ID:       "worker-fenced",
		Status:   WorkerStatusActive,
		Address:  "10.0.0.1",
		Capacity: 4,
		Port:     5000,
	}
	first, err := repo.Register(ctx, w)
	if err != nil {
		t.Fatalf("register new worker: %v", err)
	}
	if first.FencingToken != 1 {
		t.Fatalf("first fencing token: want 1, got %d", first.FencingToken)
	}

	w.Address = "10.0.0.2"
	second, err := repo.Register(ctx, w)
	if err != nil {
		t.Fatalf("register replacement worker: %v", err)
	}
	if second.FencingToken != 2 || second.Address != w.Address {
		t.Fatalf("replacement registration: want token 2 and address %q, got token %d and address %q", w.Address, second.FencingToken, second.Address)
	}

	first.Address = "10.0.0.3"
	if _, err := repo.Update(ctx, first); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale heartbeat: want ErrFenced, got %v", err)
	}

	got, err := repo.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.FencingToken != 2 || got.Address != w.Address {
		t.Fatalf("stale heartbeat changed row: token=%d address=%q", got.FencingToken, got.Address)
	}

	second.Capacity = 8
	updated, err := repo.Update(ctx, second)
	if err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if updated.FencingToken != 2 || updated.Capacity != 8 {
		t.Fatalf("current heartbeat: want token 2 and capacity 8, got token %d and capacity %d", updated.FencingToken, updated.Capacity)
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	active, err := repo.ListActive(ctx)
	if err != nil {
		t.Fatalf("list active workers: %v", err)
	}
	if len(all) != 1 || all[0].FencingToken != 2 || len(active) != 1 || active[0].FencingToken != 2 {
		t.Fatalf("list scans lost fencing token: all=%+v active=%+v", all, active)
	}
}

func TestWorkerRepository_RegisterConcurrent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	const registrations = 8
	var wg sync.WaitGroup
	results := make(chan int64, registrations)
	errs := make(chan error, registrations)
	for i := 0; i < registrations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registered, err := repo.Register(ctx, Worker{
				ID: "worker-concurrent", Status: WorkerStatusActive, Address: "10.0.0.1",
			})
			if err != nil {
				errs <- err
				return
			}
			results <- registered.FencingToken
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent registration: %v", err)
	}
	seen := make(map[int64]bool)
	for token := range results {
		if token < 1 || token > registrations || seen[token] {
			t.Errorf("unexpected or duplicate fencing token %d", token)
		}
		seen[token] = true
	}
	if len(seen) != registrations {
		t.Fatalf("want %d distinct tokens, got %d", registrations, len(seen))
	}
}

// A worker may register before its port is known: Port 0 must round-trip as a
// SQL NULL (read back as 0), and a later update with a real port must persist.
func TestWorkerRepository_NullablePortRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	created, err := repo.Create(ctx, Worker{
		ID:           "worker-p0",
		Status:       WorkerStatusActive,
		Address:      "10.0.0.1",
		Capacity:     4,
		Port:         0, // not reported yet -> stored as NULL
		FencingToken: 23,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Port != 0 {
		t.Fatalf("expected port 0 (NULL), got %d", created.Port)
	}
	if created.FencingToken != 23 {
		t.Fatalf("expected explicit fencing token 23, got %d", created.FencingToken)
	}

	got, err := repo.Get(ctx, "worker-p0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Port != 0 {
		t.Fatalf("expected port 0 read back, got %d", got.Port)
	}
	if got.FencingToken != 23 {
		t.Fatalf("expected fencing token 23 read back, got %d", got.FencingToken)
	}

	got.Port = 54321
	updated, err := repo.Update(ctx, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Port != 54321 {
		t.Fatalf("expected port 54321 after update, got %d", updated.Port)
	}
}

// MarkStaleInactive must demote only active workers whose last heartbeat is
// older than the timeout, leaving fresh workers and already-inactive rows alone.
func TestWorkerRepository_MarkStaleInactive(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	now := time.Now().UTC()
	mustCreate := func(id string, status WorkerStatus, hb time.Time) {
		if _, err := repo.Create(ctx, Worker{
			ID:             id,
			Status:         status,
			Address:        "10.0.0.1",
			Capacity:       4,
			Port:           5000,
			Last_heartbeat: hb,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	mustCreate("stale-active", WorkerStatusActive, now.Add(-30*time.Second))
	mustCreate("fresh-active", WorkerStatusActive, now.Add(-2*time.Second))
	mustCreate("stale-inactive", WorkerStatusInactive, now.Add(-30*time.Second))

	n, err := repo.MarkStaleInactive(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleInactive: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 worker reaped, got %d", n)
	}

	wantStatus := map[string]WorkerStatus{
		"stale-active":   WorkerStatusInactive, // reaped
		"fresh-active":   WorkerStatusActive,   // heartbeat still fresh
		"stale-inactive": WorkerStatusInactive, // already inactive, unchanged
	}
	for id, want := range wantStatus {
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("worker %s: expected status %q, got %q", id, want, got.Status)
		}
	}

	// A second sweep is a no-op now that the stale worker is already inactive.
	n, err = repo.MarkStaleInactive(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleInactive (2nd): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 reaped on second sweep, got %d", n)
	}
}

// inactive_since records when the reaper marked a worker inactive, and a
// heartbeat or a registration clears it. last_heartbeat is the database's
// clock whatever the caller sends: the sandbox reaper compares it with
// sandboxes.last_seen_at.
func TestWorkerRepository_InactiveSinceAndDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	inactiveSince := func(id string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT inactive_since FROM worker WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("read inactive_since of %s: %v", id, err)
		}
		return at
	}
	heardAgo := func(id string) time.Duration {
		t.Helper()
		var secs float64
		if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - last_heartbeat) FROM worker WHERE id = $1`, id).Scan(&secs); err != nil {
			t.Fatalf("read last_heartbeat of %s: %v", id, err)
		}
		return time.Duration(secs * float64(time.Second))
	}
	sweep := func() {
		t.Helper()
		if _, err := repo.MarkStaleInactive(ctx, 15*time.Second); err != nil {
			t.Fatalf("MarkStaleInactive: %v", err)
		}
	}
	makeStale := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE worker SET last_heartbeat = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}

	registered, err := repo.Register(ctx, Worker{ID: "w", Status: WorkerStatusActive, Address: "10.0.0.1", Port: 5000,
		Last_heartbeat: time.Now().Add(-time.Hour)}) // the caller's clock is ignored
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if age := heardAgo("w"); age > 5*time.Second || age < -5*time.Second {
		t.Fatalf("registration stored the caller's clock: heartbeat %s ago", age)
	}
	if at := inactiveSince("w"); at != nil {
		t.Fatalf("active worker has inactive_since %s", at)
	}

	// The reaper marks it inactive, and says when.
	makeStale("w")
	sweep()
	first := inactiveSince("w")
	if first == nil {
		t.Fatal("worker marked inactive has no inactive_since")
	}
	if age := time.Since(*first); age > 10*time.Second || age < -10*time.Second {
		t.Fatalf("inactive_since is %s ago, want about now", age)
	}
	// Later sweeps do not move it: the worker has been inactive since the first.
	time.Sleep(50 * time.Millisecond)
	sweep()
	if again := inactiveSince("w"); again == nil || !again.Equal(*first) {
		t.Fatalf("a second sweep moved inactive_since from %s to %v", first, again)
	}

	// A heartbeat brings it back: active, inactive_since cleared, the database's clock.
	registered.Last_heartbeat = time.Now().Add(-time.Hour)
	updated, err := repo.Update(ctx, registered)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if updated.Status != WorkerStatusActive {
		t.Fatalf("heartbeat left the worker %s", updated.Status)
	}
	if at := inactiveSince("w"); at != nil {
		t.Fatalf("a heartbeat left inactive_since at %s", at)
	}
	if age := heardAgo("w"); age > 5*time.Second || age < -5*time.Second {
		t.Fatalf("heartbeat stored the caller's clock: heartbeat %s ago", age)
	}

	// A registration clears it too.
	makeStale("w")
	sweep()
	if inactiveSince("w") == nil {
		t.Fatal("worker was not marked inactive again")
	}
	if _, err := repo.Register(ctx, Worker{ID: "w", Status: WorkerStatusActive, Address: "10.0.0.1", Port: 5000}); err != nil {
		t.Fatalf("register again: %v", err)
	}
	if at := inactiveSince("w"); at != nil {
		t.Fatalf("a registration left inactive_since at %s", at)
	}
}

// A heartbeat that matches no row is refused in one of two ways, told apart by
// the token the database holds: ErrWorkerUnknown when it has no such worker or
// is behind the token presented (the database is late: row deleted, backup
// restored), ErrFenced when it is ahead (an older process).
func TestWorkerRepository_UpdateTellsUnknownFromFenced(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	beat := func(id string, token int64) error {
		_, err := repo.Update(ctx, Worker{ID: id, Status: WorkerStatusActive, Address: "10.0.0.1", Port: 5000, FencingToken: token})
		return err
	}
	setToken := func(id string, token int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE worker SET fencing_token = $2 WHERE id = $1`, id, token); err != nil {
			t.Fatalf("set token of %s: %v", id, err)
		}
	}

	if err := beat("absent", 1); !errors.Is(err, ErrWorkerUnknown) {
		t.Fatalf("no row: want ErrWorkerUnknown, got %v", err)
	}
	if _, err := repo.Get(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused heartbeat created the row: %v", err)
	}

	registered, err := repo.Register(ctx, Worker{ID: "w", Status: WorkerStatusActive, Address: "10.0.0.1", Port: 5000})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	token := registered.FencingToken

	// The database's token, against the one presented.
	for _, tc := range []struct {
		name    string
		current int64
		want    error
	}{
		{"database behind (token lost)", token - 1, ErrWorkerUnknown},
		{"database far behind (reset to 0)", 0, ErrWorkerUnknown},
		{"equal", token, nil},
		{"database ahead (older process)", token + 1, ErrFenced},
		{"database far ahead", token + 10, ErrFenced},
	} {
		setToken("w", tc.current)
		if err := beat("w", token); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, err)
		}
		got, err := repo.Get(ctx, "w")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.FencingToken != tc.current {
			t.Errorf("%s: a heartbeat changed the token from %d to %d", tc.name, tc.current, got.FencingToken)
		}
	}
}

// Register hands out a token above the persisted one and above the one the
// registering process presents, so a database that lost its tokens never reuses
// a token an older process held.
func TestWorkerRepository_RegisterOutrunsThePresentedToken(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	register := func(id string, presented int64) int64 {
		t.Helper()
		w, err := repo.Register(ctx, Worker{ID: id, Status: WorkerStatusActive, Address: "10.0.0.1", Port: 5000, FencingToken: presented})
		if err != nil {
			t.Fatalf("register %s presenting %d: %v", id, presented, err)
		}
		return w.FencingToken
	}

	// A new row: above the token presented, 1 for a fresh process.
	if got := register("fresh", 0); got != 1 {
		t.Errorf("fresh process: want token 1, got %d", got)
	}
	if got := register("lost-row", 7); got != 8 {
		t.Errorf("row lost, process held 7: want token 8, got %d", got)
	}
	// An existing row: above both.
	if got := register("fresh", 0); got != 2 {
		t.Errorf("replacement presenting nothing: want token 2, got %d", got)
	}
	if got := register("fresh", 9); got != 10 {
		t.Errorf("database behind a process that held 9: want token 10, got %d", got)
	}
	if got := register("fresh", 4); got != 11 {
		t.Errorf("process behind the database: want token 11, got %d", got)
	}
	if got := register("fresh", -3); got != 12 {
		t.Errorf("negative token presented: want token 12, got %d", got)
	}
}
