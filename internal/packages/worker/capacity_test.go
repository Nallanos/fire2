package worker

import "testing"

func TestComputeCapacity(t *testing.T) {
	t.Setenv("WORKER_MAX_SANDBOXES", "")

	tests := []struct {
		name        string
		cpu, memMiB int
		want        int
	}{
		{"memory is the limit", 4, 1024, 2}, // (1024-384)/320 = 2
		{"cpu is the limit", 2, 8192, 2},    // 2/1
		{"too small for one sandbox", 1, 300, 0},
		{"below reserve", 1, 64, 0},
		// The 1 GiB / 2 vCPU workers (MemTotal 952 MiB): (952-384)/320 = 1.
		// Two microVMs would not fit next to the OS, and there is no swap.
		{"the 1 GiB workers", 2, 952, 1},
		{"just enough for one", 1, 704, 1}, // (704-384)/320 = 1
		{"just short of one", 1, 703, 0},   // (703-384)/320 = 0
		{"a larger worker", 16, 16384, 16}, // (16384-384)/320 = 50, cpu limits
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := computeCapacity(tt.cpu, tt.memMiB); got != tt.want {
				t.Fatalf("computeCapacity(%d, %d) = %d, want %d", tt.cpu, tt.memMiB, got, tt.want)
			}
		})
	}
}

func TestComputeCapacity_EnvOverrideNeverExceedsBudget(t *testing.T) {
	t.Setenv("WORKER_MAX_SANDBOXES", "2")
	if got := computeCapacity(8, 8192); got != 2 {
		t.Fatalf("override lower than budget: got %d, want 2", got)
	}
	t.Setenv("WORKER_MAX_SANDBOXES", "50")
	if got := computeCapacity(2, 8192); got != 2 {
		t.Fatalf("override above budget must be capped: got %d, want 2", got)
	}
}
