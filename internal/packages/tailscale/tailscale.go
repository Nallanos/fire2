// Package tailscale shells out to the local tailscale CLI so a service can
// discover its own tailnet identity instead of relying on a static IP baked
// into deploy config — the latter goes stale the moment a host is rebuilt
// or moved to a new provider. Used by both cmd/worker and cmd/api.
package tailscale

import (
	"errors"
	"os/exec"
	"strings"
)

// ErrEmptyIP is returned when `tailscale ip -4` succeeds but prints nothing.
var ErrEmptyIP = errors.New("tailscale ip -4: empty output")

// IPv4 returns this machine's own tailnet IPv4 address. Returns an error
// when the tailscale binary is missing or the host hasn't joined a tailnet
// (e.g. local dev) so callers can fall back to their previous behavior.
func IPv4() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", ErrEmptyIP
	}
	return ip, nil
}
