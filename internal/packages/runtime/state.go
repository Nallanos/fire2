package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
)

// apiClient talks HTTP to Firecracker's API socket.
func apiClient(socketPath string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// One request every few seconds per VM: no point keeping
		// connections to sockets that may vanish with the VM.
		DisableKeepAlives: true,
	}}
}

// State samples the VM's instance info (GET / on Firecracker's API) and maps
// its state ("Not started" | "Running" | "Paused") onto the closed
// SandboxState vocabulary. An error means the API could not be queried; the
// caller decides whether that is UNKNOWN or DEAD (see Wait).
func (c *Client) State(ctx context.Context) (orchestratorv1.SandboxState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, err
	}
	resp, err := c.api.Do(req)
	if err != nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("instance info: %s", resp.Status)
	}
	var info struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("instance info: %w", err)
	}

	switch info.State {
	case "Running":
		return orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING, nil
	case "Paused":
		return orchestratorv1.SandboxState_SANDBOX_STATE_PAUSED, nil
	case "Not started":
		return orchestratorv1.SandboxState_SANDBOX_STATE_STARTING, nil
	default:
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("unexpected instance state %q", info.State)
	}
}
