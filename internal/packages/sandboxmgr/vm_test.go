package sandboxmgr

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeStart blocks until stop is called, like a Machine.Start stuck waiting
// for a Firecracker process that only exits when killed.
type fakeStart struct {
	killed chan struct{}
}

func newFakeStart() *fakeStart { return &fakeStart{killed: make(chan struct{})} }

func (f *fakeStart) start() error {
	<-f.killed
	return errors.New("firecracker exited")
}

func (f *fakeStart) stop() error {
	close(f.killed)
	return nil
}

func TestStartBoundedReturnsStartResult(t *testing.T) {
	want := errors.New("bad config")
	stopped := false
	err := startBounded(context.Background(), func() error { return want }, func() error { stopped = true; return nil }, time.Second)
	if !errors.Is(err, want) || stopped {
		t.Fatalf("err = %v, stopped = %v: want start's own error and no stop", err, stopped)
	}
	if err := startBounded(context.Background(), func() error { return nil }, func() error { return nil }, time.Second); err != nil {
		t.Fatalf("successful start: %v", err)
	}
}

func TestStartBoundedKillsAStuckStart(t *testing.T) {
	f := newFakeStart()
	began := time.Now()
	err := startBounded(context.Background(), f.start, f.stop, 20*time.Millisecond)
	if err == nil {
		t.Fatal("a stuck start must fail")
	}
	select {
	case <-f.killed:
	default:
		t.Fatal("the VM was not stopped")
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("returned after %s, want about the timeout", elapsed)
	}
}

func TestStartBoundedAbortsOnShutdown(t *testing.T) {
	f := newFakeStart()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := startBounded(ctx, f.start, f.stop, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
