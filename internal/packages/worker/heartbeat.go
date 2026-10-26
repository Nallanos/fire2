package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultHeartbeatTimeout = 15 * time.Second

const (
	// While the orchestrator keeps refusing the heartbeats, the wait between
	// two of them doubles, up to this.
	heartbeatRefusedBackoffMax = time.Minute
	// A refusal streak is logged when it starts, then at most this often.
	heartbeatRefusedLogEvery = 5 * time.Minute
	// RunHeartbeat gives up once no heartbeat was accepted for this long.
	heartbeatRefusedGiveUp = 10 * time.Minute
)

// ExitHeartbeatRefused is the exit status of a worker whose heartbeats the
// orchestrator refused for good (EX_CONFIG in sysexits.h). The systemd unit
// lists it in RestartPreventExitStatus: a restart would only be refused
// again, so the unit stays failed instead of looping.
const ExitHeartbeatRefused = 78

// ErrHeartbeatRefused is what RunHeartbeat returns when it gives up.
var ErrHeartbeatRefused = errors.New("orchestrator refuses this worker's heartbeats")

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
//
// It returns nil when ctx is done. If the orchestrator refuses every heartbeat
// for good (see refusedForGood) for heartbeatRefusedGiveUp, it returns an
// error wrapping ErrHeartbeatRefused: nothing this process can do will change
// the answer.
func (w *WorkerService) RunHeartbeat(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	var streak refusals
	timer := time.NewTimer(0) // the first heartbeat goes out at once
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		err := w.sendHeartbeat(ctx)
		now := nowFunc()
		switch st, refused := refusedForGood(err); {
		case refused:
			logIt, giveUp := streak.record(now)
			refusedFor := now.Sub(streak.since).Round(time.Second)
			if giveUp {
				return fmt.Errorf("%w for %s: %s", ErrHeartbeatRefused, refusedFor, st.Message())
			}
			if logIt {
				slog.Error("orchestrator refuses this worker's heartbeats, backing off",
					"code", st.Code().String(),
					"reason", st.Message(),
					"refused_for", refusedFor.String(),
					"next_attempt_in", streak.wait(interval).String(),
					"hint", "an old worker against a newer orchestrator: redeploy the worker")
			}
		case err != nil:
			slog.Error("worker heartbeat update failed", "err", err)
		default:
			if streak.count > 0 {
				slog.Info("orchestrator accepts this worker's heartbeats again",
					"refused_for", now.Sub(streak.since).Round(time.Second).String())
			}
			streak = refusals{}
		}
		timer.Reset(streak.wait(interval))
	}
}

func (w *WorkerService) sendHeartbeat(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout)
	defer cancel()
	_, err := w.UpdateWorker(hctx)
	return err
}

// refusedForGood reports whether err is the orchestrator refusing what this
// worker sends, which sending it again cannot fix: a protocol or configuration
// mismatch, typically a worker older than the orchestrator. An unreachable
// orchestrator, a timeout or a stale fencing token are not.
func refusedForGood(err error) (*status.Status, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return nil, false
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.Unimplemented, codes.PermissionDenied, codes.Unauthenticated:
		return st, true
	}
	return nil, false
}

// refusals follows a streak of heartbeats refused for good.
type refusals struct {
	count   int
	since   time.Time // first refusal of the streak
	lastLog time.Time
}

// record adds a refusal at now. logIt is true for the first refusal of the
// streak and then every heartbeatRefusedLogEvery; giveUp once the streak has
// lasted heartbeatRefusedGiveUp.
func (r *refusals) record(now time.Time) (logIt, giveUp bool) {
	if r.count == 0 {
		r.since = now
	}
	r.count++
	if r.count == 1 || now.Sub(r.lastLog) >= heartbeatRefusedLogEvery {
		r.lastLog = now
		logIt = true
	}
	return logIt, now.Sub(r.since) >= heartbeatRefusedGiveUp
}

// wait is the time before the next heartbeat: interval, doubled for each
// refusal of the streak, up to heartbeatRefusedBackoffMax (never below
// interval).
func (r *refusals) wait(interval time.Duration) time.Duration {
	d := interval
	for i := 0; i < r.count && d < heartbeatRefusedBackoffMax; i++ {
		d *= 2
	}
	return max(min(d, heartbeatRefusedBackoffMax), interval)
}
