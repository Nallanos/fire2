package orchestrator

import (
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

func TestStrongRetryPolicy_BackoffSequence(t *testing.T) {
	policy := &StrongRetryPolicy{}
	const delta = 100 * time.Millisecond

	tests := []struct {
		priorErrors int
		wantBackoff time.Duration
	}{
		{priorErrors: 0, wantBackoff: 2 * time.Second},
		{priorErrors: 1, wantBackoff: 4 * time.Second},
		{priorErrors: 2, wantBackoff: 8 * time.Second},
		{priorErrors: 3, wantBackoff: 16 * time.Second},
		{priorErrors: 4, wantBackoff: 30 * time.Second}, // 32s capped to 30s
		{priorErrors: 5, wantBackoff: 30 * time.Second}, // 64s capped to 30s
	}

	for _, tt := range tests {
		job := &rivertype.JobRow{
			Errors: make([]rivertype.AttemptError, tt.priorErrors),
		}

		before := time.Now()
		next := policy.NextRetry(job)
		after := time.Now()

		earliest := before.Add(tt.wantBackoff - delta)
		latest := after.Add(tt.wantBackoff + delta)

		if next.Before(earliest) || next.After(latest) {
			t.Errorf("priorErrors=%d: got backoff ~%v, want ~%v",
				tt.priorErrors, next.Sub(before).Round(time.Millisecond), tt.wantBackoff)
		}
	}
}

func TestStrongRetryPolicy_ReturnTimeIsAfterNow(t *testing.T) {
	policy := &StrongRetryPolicy{}
	job := &rivertype.JobRow{Errors: nil}

	before := time.Now()
	next := policy.NextRetry(job)

	if !next.After(before) {
		t.Fatalf("expected NextRetry to be after time.Now(), got %v (before=%v)", next, before)
	}
}
