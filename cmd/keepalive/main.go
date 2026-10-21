package main

import (
	"log/slog"
	"time"

	"github/nallanos/fire2/internal/app"
)

func main() {
	app.SetupLogger("keepalive")

	slog.Info("keepalive started")
	for {
		time.Sleep(1 * time.Hour)
	}
}
