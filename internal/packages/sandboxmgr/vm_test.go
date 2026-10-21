package sandboxmgr

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitRunningReturnsOnceRunning(t *testing.T) {
	var calls atomic.Int32
	state := func(context.Context) (State, error) {
		switch calls.Add(1) {
		case 1:
			return 0, errors.New("api socket not there yet")
		case 2:
			return StateStarting, nil
		default:
			return StateRunning, nil
		}
	}
	if err := waitRunning(context.Background(), state, nil, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitRunning: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("polled %d times, want 3", n)
	}
}

func TestWaitRunningTimesOut(t *testing.T) {
	state := func(context.Context) (State, error) { return StateStarting, nil }
	began := time.Now()
	err := waitRunning(context.Background(), state, nil, 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a VM that never runs must fail")
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("returned after %s, want about the timeout", elapsed)
	}
}

func TestWaitRunningFailsWhenTheVMIsGone(t *testing.T) {
	gone := make(chan struct{})
	close(gone)
	state := func(context.Context) (State, error) { return 0, errors.New("connection refused") }
	if err := waitRunning(context.Background(), state, gone, time.Hour, time.Millisecond); err == nil {
		t.Fatal("a VM gone while booting must fail")
	}
}

func TestWaitRunningAbortsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := func(context.Context) (State, error) { return StateStarting, nil }
	if err := waitRunning(ctx, state, nil, time.Hour, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
