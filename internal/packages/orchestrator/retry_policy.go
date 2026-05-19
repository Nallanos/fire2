package orchestrator

import (
	"time"

	"github.com/riverqueue/river/rivertype"
)

// StrongRetryPolicy uses exponential backoff: min(2^attempt seconds, 30s).
// With MaxAttempts=5: 2+4+8+16+30 = 60s worst-case total — fits the 45s HTTP window.
type StrongRetryPolicy struct{}

func (p *StrongRetryPolicy) NextRetry(job *rivertype.JobRow) time.Time {
	attempt := len(job.Errors) + 1
	backoff := time.Duration(1<<attempt) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	return time.Now().UTC().Add(backoff)
}
