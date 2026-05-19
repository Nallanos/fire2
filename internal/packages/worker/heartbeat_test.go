package worker

import (
	"testing"
	"time"
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
