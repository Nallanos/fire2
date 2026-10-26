package logging

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in     string
		level  slog.Level
		wantOK bool
	}{
		{"", slog.LevelInfo, true},
		{"info", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"Warn", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"verbose", slog.LevelInfo, false},
	}
	for _, tt := range tests {
		level, ok := parseLogLevel(tt.in)
		if level != tt.level || ok != tt.wantOK {
			t.Errorf("parseLogLevel(%q) = (%v, %v), want (%v, %v)", tt.in, level, ok, tt.level, tt.wantOK)
		}
	}
}
