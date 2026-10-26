// Package logging sets up the process-wide structured logger shared by every
// binary. It depends on the standard library only: fire2-shim imports it, and
// must not drag the orchestrator's dependencies into the process that must
// never crash.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// SetupLogger installs the process-wide slog default: one JSON object per
// line on stderr (so journald keeps it intact for shipping), tagged with the
// given service name. The level comes from FIRE2_LOG_LEVEL
// (debug|info|warn|error, case-insensitive), defaulting to info.
//
// slog.SetDefault also routes the std "log" package through the new handler,
// so any stray log.Printf still ends up as JSON.
func SetupLogger(service string) {
	raw := os.Getenv("FIRE2_LOG_LEVEL")
	level, ok := parseLogLevel(raw)

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h).With("service", service))

	if !ok {
		slog.Warn("unknown FIRE2_LOG_LEVEL, using info", "value", raw)
	}
}

// parseLogLevel maps a level name to a slog.Level. An empty value is the
// default (info) and counts as valid; an unknown value falls back to info and
// reports ok=false.
func parseLogLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}
