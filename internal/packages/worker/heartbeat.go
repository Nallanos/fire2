package worker

import (
	"context"
	"log"
	"time"
)

const defaultHeartbeatTimeout = 15 * time.Second

var nowFunc = time.Now

func HeartbeatExpired(lastHeartbeat time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultHeartbeatTimeout
	}
	if lastHeartbeat.IsZero() {
		return true
	}

	return nowFunc().Sub(lastHeartbeat) > timeout
}

// RunHeartbeat periodically updates this worker's heartbeat and runtime metrics in the database.
func (w *WorkerService) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	w.sendHeartbeat(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sendHeartbeat(ctx)
		}
	}
}

func (w *WorkerService) sendHeartbeat(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout)
	defer cancel()
	if _, err := w.UpdateWorker(hctx); err != nil {
		log.Printf("worker heartbeat update failed: %v", err)
	}
}
