//go:build integration

package divergence

import (
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Dozens of heartbeats a second would drown the output of a failing
	// scenario. Warnings and errors still show.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	os.Exit(m.Run())
}
