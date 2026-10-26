package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestHeartbeatExpired(t *testing.T) {
	fixedNow := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	defer func() { nowFunc = originalNow }()

	within := fixedNow.Add(-5 * time.Second)
	if HeartbeatExpired(within, 10*time.Second) {
		t.Fatalf("expected heartbeat to be fresh")
	}

	expired := fixedNow.Add(-20 * time.Second)
	if !HeartbeatExpired(expired, 10*time.Second) {
		t.Fatalf("expected heartbeat to be expired")
	}
}

func TestHeartbeatExpiredZeroValue(t *testing.T) {
	fixedNow := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	defer func() { nowFunc = originalNow }()

	if !HeartbeatExpired(time.Time{}, 10*time.Second) {
		t.Fatalf("expected zero heartbeat to be expired")
	}
}

func TestHeartbeatExpiredDefaultTimeout(t *testing.T) {
	fixedNow := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	defer func() { nowFunc = originalNow }()

	last := fixedNow.Add(-defaultHeartbeatTimeout / 2)
	if HeartbeatExpired(last, 0) {
		t.Fatalf("expected default timeout to treat heartbeat as fresh")
	}
}

func TestRefusedForGood(t *testing.T) {
	refusal := status.Error(codes.InvalidArgument, "fencing_token is required")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"invalid argument":    {refusal, true},
		"unimplemented":       {status.Error(codes.Unimplemented, "x"), true},
		"permission denied":   {status.Error(codes.PermissionDenied, "x"), true},
		"unauthenticated":     {status.Error(codes.Unauthenticated, "x"), true},
		"wrapped refusal":     {fmt.Errorf("heartbeat: %w", refusal), true},
		"unavailable":         {status.Error(codes.Unavailable, "x"), false},
		"deadline exceeded":   {status.Error(codes.DeadlineExceeded, "x"), false},
		"stale fencing token": {status.Error(codes.FailedPrecondition, "x"), false},
		"not a grpc error":    {errors.New("boom"), false},
		"no error":            {nil, false},
	} {
		if _, got := refusedForGood(tc.err); got != tc.want {
			t.Errorf("%s: refusedForGood = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRefusalsWaitDoublesUpToTheCap(t *testing.T) {
	for _, tc := range []struct {
		count    int
		interval time.Duration
		want     time.Duration
	}{
		{0, 5 * time.Second, 5 * time.Second},
		{1, 5 * time.Second, 10 * time.Second},
		{2, 5 * time.Second, 20 * time.Second},
		{3, 5 * time.Second, 40 * time.Second},
		{4, 5 * time.Second, time.Minute},
		{50, 5 * time.Second, time.Minute},
		// An interval above the cap is never shortened.
		{3, 2 * time.Minute, 2 * time.Minute},
	} {
		r := refusals{count: tc.count}
		if got := r.wait(tc.interval); got != tc.want {
			t.Errorf("count=%d interval=%s: wait = %s, want %s", tc.count, tc.interval, got, tc.want)
		}
	}
}

func TestRefusalsLogThenThrottleThenGiveUp(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var r refusals

	for _, tc := range []struct {
		after            time.Duration
		wantLog, wantEnd bool
	}{
		{0, true, false},                 // the streak starts: logged
		{10 * time.Second, false, false}, // then quiet
		{4 * time.Minute, false, false},  // still within the throttle
		{5 * time.Minute, true, false},   // logged again
		{9*time.Minute + 59*time.Second, false, false},
		{10 * time.Minute, true, true}, // 5 min since the last log; given up
	} {
		logIt, giveUp := r.record(start.Add(tc.after))
		if logIt != tc.wantLog || giveUp != tc.wantEnd {
			t.Errorf("after %s: record = (log %v, giveUp %v), want (%v, %v)", tc.after, logIt, giveUp, tc.wantLog, tc.wantEnd)
		}
	}
}

// scriptedClientStub returns errs in order from ReportWorkerHeartbeat, then
// accepts every heartbeat.
type scriptedClientStub struct {
	heartbeatClientStub
	errs       []error
	calls      int
	onAccepted func()
}

func (c *scriptedClientStub) ReportWorkerHeartbeat(ctx context.Context, req *orchestratorv1.WorkerHeartbeat, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	c.calls++
	if c.calls <= len(c.errs) {
		return nil, c.errs[c.calls-1]
	}
	if c.onAccepted != nil {
		c.onAccepted()
	}
	return c.heartbeatClientStub.ReportWorkerHeartbeat(ctx, req, opts...)
}

func newScriptedWorker(client *scriptedClientStub) *WorkerService {
	client.registrationToken = 1
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-1", "127.0.0.1")
	w.SetWorkerBudget(1, 512)
	return w
}

func refusedErrs(n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = status.Error(codes.InvalidArgument, "fencing_token is required")
	}
	return errs
}

// Every call to the clock moves it one minute on, so the 10 minutes of
// refusals pass in a few iterations.
func TestRunHeartbeatGivesUpWhenTheOrchestratorKeepsRefusing(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { now = now.Add(time.Minute); return now }
	defer func() { nowFunc = originalNow }()

	// The deadline only turns a loop that never gives up into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &scriptedClientStub{errs: refusedErrs(100)}
	err := newScriptedWorker(client).RunHeartbeat(ctx, 100*time.Microsecond)

	if !errors.Is(err, ErrHeartbeatRefused) {
		t.Fatalf("RunHeartbeat = %v, want ErrHeartbeatRefused", err)
	}
	if client.calls < 2 || client.calls >= 100 {
		t.Fatalf("heartbeats before giving up = %d, want a few retries", client.calls)
	}
}

func TestRunHeartbeatRecoversWhenTheOrchestratorAcceptsAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &scriptedClientStub{errs: refusedErrs(3), onAccepted: cancel}

	if err := newScriptedWorker(client).RunHeartbeat(ctx, 100*time.Microsecond); err != nil {
		t.Fatalf("RunHeartbeat = %v, want nil once accepted", err)
	}
	if client.calls != 4 {
		t.Fatalf("heartbeats = %d, want 3 refused + 1 accepted", client.calls)
	}
}

// Transient failures never end the loop, however long they last.
func TestRunHeartbeatKeepsGoingOnTransientErrors(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { now = now.Add(time.Hour); return now }
	defer func() { nowFunc = originalNow }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := make([]error, 30)
	for i := range errs {
		errs[i] = status.Error(codes.Unavailable, "orchestrator down")
	}
	client := &scriptedClientStub{errs: errs, onAccepted: cancel}

	if err := newScriptedWorker(client).RunHeartbeat(ctx, 100*time.Microsecond); err != nil {
		t.Fatalf("RunHeartbeat = %v, want nil: Unavailable is not a refusal", err)
	}
	if client.calls != 31 {
		t.Fatalf("heartbeats = %d, want 30 failures + 1 accepted", client.calls)
	}
}

// ServeGRPC must hand the refusal back to main, not just stop serving.
func TestServeGRPCReturnsWhenTheHeartbeatGivesUp(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	originalNow := nowFunc
	nowFunc = func() time.Time { now = now.Add(time.Minute); return now }
	defer func() { nowFunc = originalNow }()
	t.Setenv(heartbeatIntervalEnv, "100us")

	client := &scriptedClientStub{errs: refusedErrs(100)}
	srv := NewWorkerGRPCServer(newScriptedWorker(client))
	done := make(chan error, 1)
	go func() { done <- ServeGRPC("127.0.0.1:0", srv) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrHeartbeatRefused) {
			t.Fatalf("ServeGRPC = %v, want ErrHeartbeatRefused", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeGRPC still serving after the heartbeat gave up")
	}
}
