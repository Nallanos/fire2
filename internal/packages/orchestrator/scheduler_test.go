package orchestrator

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/riverqueue/river/rivertype"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

func TestSchedulerWeightedRandomDistribution(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	scheduler := NewSchedulerWithRand(rng)

	candidates := []WorkerCandidate{
		workerCandidate("worker-a", 25, 100, 25, 100),
		workerCandidate("worker-b", 75, 100, 75, 100),
	}

	counts := map[string]int{}
	iterations := 10000
	for i := 0; i < iterations; i++ {
		picked, err := scheduler.ChooseLeastUsedWorker(candidates)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[picked.Worker.ID]++
	}

	countA := counts["worker-a"]
	countB := counts["worker-b"]
	if countB == 0 {
		t.Fatalf("worker-b was never selected")
	}

	ratio := float64(countA) / float64(countB)
	if ratio < 2.5 || ratio > 3.5 {
		t.Fatalf("expected ~3x selection ratio, got %.2f (a=%d b=%d)", ratio, countA, countB)
	}
}

func TestSchedulerSingleWorker(t *testing.T) {
	scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(1)))
	candidates := []WorkerCandidate{workerCandidate("worker-a", 10, 100, 10, 100)}

	picked, err := scheduler.ChooseLeastUsedWorker(candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.Worker.ID != "worker-a" {
		t.Fatalf("expected worker-a, got %s", picked.Worker.ID)
	}
}

func TestSchedulerAllWorkersAtCapacity(t *testing.T) {
	scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(7)))
	candidates := []WorkerCandidate{
		workerCandidate("worker-a", 100, 100, 100, 100),
		workerCandidate("worker-b", 100, 100, 100, 100),
	}

	_, err := scheduler.ChooseLeastUsedWorker(candidates)
	if !errors.Is(err, ErrClusterFull) {
		t.Fatalf("expected ErrClusterFull, got %v", err)
	}
}

func TestSchedulerSkipsSaturatedWorker(t *testing.T) {
	scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(3)))
	candidates := []WorkerCandidate{
		workerCandidate("full-cpu", 100, 100, 10, 100),
		workerCandidate("full-mem", 10, 100, 100, 100),
		workerCandidate("free", 90, 100, 90, 100),
	}

	for i := 0; i < 50; i++ {
		picked, err := scheduler.ChooseLeastUsedWorker(candidates)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if picked.Worker.ID != "free" {
			t.Fatalf("picked saturated worker %s", picked.Worker.ID)
		}
	}
}

// cpu_usage is the machine's load in percent and cpu_budget a number of cores:
// load is judged against 100, not against the cores. A 2-core worker used to be
// "full" at 2% load.
func TestSchedulerCPUUsageIsAPercentageNotACoreCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cpuUsage int32 // percent
		cores    int32
		wantFull bool
	}{
		{"2 cores almost idle", 2, 2, false},
		{"4 cores almost idle", 4, 4, false},
		{"2 cores at 50%", 50, 2, false},
		{"4 cores at 99%", 99, 4, false},
		{"2 cores at 100%", 100, 2, true},
		{"4 cores at 100%", 100, 4, true},
	} {
		scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(5)))
		// Memory is plentiful (512 of 4096 MiB): only the CPU can fill the worker.
		_, err := scheduler.ChooseLeastUsedWorker([]WorkerCandidate{workerCandidate("w", tc.cpuUsage, tc.cores, 512, 4096)})
		switch {
		case tc.wantFull && !errors.Is(err, ErrClusterFull):
			t.Errorf("%s: want ErrClusterFull, got %v", tc.name, err)
		case !tc.wantFull && err != nil:
			t.Errorf("%s: want a place, got %v", tc.name, err)
		}
	}
}

// The weights follow the load in percent too, whatever the number of cores:
// the idler worker gets the sandbox more often.
func TestSchedulerWeightsFollowTheLoadInPercent(t *testing.T) {
	scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(11)))
	candidates := []WorkerCandidate{
		workerCandidate("idle-2-cores", 10, 2, 512, 4096),
		workerCandidate("busy-8-cores", 90, 8, 512, 4096),
	}
	idle := 0
	const iterations = 5000
	for i := 0; i < iterations; i++ {
		picked, err := scheduler.ChooseLeastUsedWorker(candidates)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if picked.Worker.ID == "idle-2-cores" {
			idle++
		}
	}
	// Weights 0.7*0.9+0.3*0.875 against 0.7*0.1+0.3*0.875: about 0.73 of the picks.
	if share := float64(idle) / iterations; share < 0.68 || share > 0.78 {
		t.Fatalf("the idler worker got %.2f of the picks, want about 0.73", share)
	}
}

func TestJobFailedClusterFull(t *testing.T) {
	job := &rivertype.JobRow{Errors: []rivertype.AttemptError{
		{Error: "max attempts exhausted: choose worker: " + ErrClusterFull.Error()},
	}}
	if !jobFailedClusterFull(job) {
		t.Fatal("expected cluster-full match")
	}
	if jobFailedClusterFull(&rivertype.JobRow{Errors: []rivertype.AttemptError{{Error: "boom"}}}) {
		t.Fatal("unexpected match")
	}
	if jobFailedClusterFull(nil) {
		t.Fatal("nil job must not match")
	}
}

func TestSchedulerEmptyCandidates(t *testing.T) {
	scheduler := NewSchedulerWithRand(rand.New(rand.NewSource(2)))
	_, err := scheduler.ChooseLeastUsedWorker(nil)
	if err == nil {
		t.Fatalf("expected error for empty candidates")
	}
}

func workerCandidate(id string, cpuUsage, cpuBudget, memUsage, memBudget int32) WorkerCandidate {
	return WorkerCandidate{
		Worker: workerpkg.Worker{
			ID:     id,
			Status: workerpkg.WorkerStatusActive,
		},
		Info: &workerv1.GetWorkerInfoResponse{
			Id:        id,
			Status:    "active",
			CpuUsage:  cpuUsage,
			CpuBudget: cpuBudget,
			MemUsage:  memUsage,
			MemBudget: memBudget,
		},
	}
}
