package orchestrator

import (
	"math"
	"math/rand"
	"time"

	"github.com/riverqueue/river/rivertype"
)

const (
	retryBaseDelay = 60 * time.Second
	retryMaxDelay  = 10 * time.Minute
)

// StrongRetryPolicy is an exponential backoff with jitter: min(60s * 2^attempt, 10min).
type StrongRetryPolicy struct{}

func (p *StrongRetryPolicy) NextRetry(job *rivertype.JobRow) time.Time {
	attempt := job.Attempt
	if attempt < 1 {
		attempt = 1
	}

	delay := float64(retryBaseDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(retryMaxDelay) {
		delay = float64(retryMaxDelay)
	}

	// ±25% jitter
	jitter := (rand.Float64()*0.5 - 0.25) * delay
	total := time.Duration(delay + jitter)
	if total < time.Second {
		total = time.Second
	}

	return time.Now().Add(total)
}
