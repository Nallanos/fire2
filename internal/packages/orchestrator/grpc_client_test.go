package orchestrator

import "testing"

// normalizeWorkerAddress must never invent a port: a worker that hasn't
// reported its ephemeral port (port 0) is skipped (returns ""), while an
// address that already embeds a port is used as-is.
func TestNormalizeWorkerAddress(t *testing.T) {
	cases := []struct {
		name    string
		address string
		port    int32
		want    string
	}{
		{"host and port", "10.0.0.1", 50051, "10.0.0.1:50051"},
		{"trims whitespace", "  10.0.0.1  ", 7000, "10.0.0.1:7000"},
		{"embedded port wins", "10.0.0.1:6000", 0, "10.0.0.1:6000"},
		{"no port reported is skipped", "10.0.0.1", 0, ""},
		{"negative port is skipped", "10.0.0.1", -1, ""},
		{"empty address and no port is skipped", "", 0, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeWorkerAddress(tc.address, tc.port); got != tc.want {
				t.Fatalf("normalizeWorkerAddress(%q, %d) = %q, want %q", tc.address, tc.port, got, tc.want)
			}
		})
	}
}
