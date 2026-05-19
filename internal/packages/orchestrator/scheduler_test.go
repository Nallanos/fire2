package orchestrator

import (
	"math/rand"
	"testing"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
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

	picked, err := scheduler.ChooseLeastUsedWorker(candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked.Worker.ID != "worker-a" && picked.Worker.ID != "worker-b" {
		t.Fatalf("unexpected worker selected: %s", picked.Worker.ID)
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
		Worker: db.Worker{
			ID:     id,
			Status: "active",
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
