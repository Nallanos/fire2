package worker

import "testing"

func TestComputeCapacity(t *testing.T) {
	t.Setenv("WORKER_MAX_SANDBOXES", "")

	tests := []struct {
		name        string
		cpu, memMiB int
		want        int
	}{
		{"memory is the limit", 4, 1024, 3}, // (1024-128)/256 = 3
		{"cpu is the limit", 2, 8192, 2},    // 2/1
		{"too small for one sandbox", 1, 300, 0},
		{"below reserve", 1, 64, 0},
		{"one sandbox", 1, 500, 1},
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
