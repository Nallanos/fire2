package worker

import "time"

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
