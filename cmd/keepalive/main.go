package main

import (
	"log/slog"
	"time"

	"github/nallanos/fire2/internal/logging"
)

func main() {
	logging.SetupLogger("keepalive")

	slog.Info("keepalive started")
	for {
		time.Sleep(1 * time.Hour)
	}
}
