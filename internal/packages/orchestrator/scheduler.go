package orchestrator

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

var ErrNoWorkerCandidates = errors.New("no worker candidates available")

// cpuUsageScale is what a worker's cpu_usage is a part of: it is the machine's
// load in percent (0 to 100, from the load average over the core count: see
// worker.readCPUUsagePercent), not a number of cores. It is therefore compared
// to 100, never to cpu_budget, which counts cores: dividing one by the other
// judged a 2-core worker full at 2% load. Memory is different, mem_usage and
// mem_budget are both in MiB.
const cpuUsageScale = 100

// ErrClusterFull is returned when at least one active worker is reachable but
// none has CPU and memory budget left. Unlike ErrNoWorkerCandidates, retrying
// immediately won't help, and callers should surface it as a 503.
var ErrClusterFull = errors.New("no worker capacity available")

type WorkerCandidate struct {
	Worker workerpkg.Worker
	Info   *workerv1.GetWorkerInfoResponse
}

type Scheduler struct {
	rng *rand.Rand
}

func NewScheduler() *Scheduler {
	return &Scheduler{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func NewSchedulerWithRand(rng *rand.Rand) *Scheduler {
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Scheduler{rng: rng}
}

func (s *Scheduler) ChooseLeastUsedWorker(candidates []WorkerCandidate) (WorkerCandidate, error) {
	if len(candidates) == 0 {
		return WorkerCandidate{}, ErrNoWorkerCandidates
	}

	// Only consider active workers that still have budget left. If active
	// workers exist but every one of them is saturated, the fleet is full.
	hasActiveWorker := false
	var withCapacity []WorkerCandidate
	for _, c := range candidates {
		if c.Info == nil || !isActiveWorker(c) {
			continue
		}
		hasActiveWorker = true
		if hasCapacity(c.Info) {
			withCapacity = append(withCapacity, c)
		}
	}
	if hasActiveWorker && len(withCapacity) == 0 {
		return WorkerCandidate{}, ErrClusterFull
	}

	bestActive, hasActive := s.pickWeightedRandom(withCapacity, true)
	if hasActive {
		return bestActive, nil
	}

	bestAny, ok := s.pickWeightedRandom(candidates, false)
	if !ok {
		return WorkerCandidate{}, ErrNoWorkerCandidates
	}

	return bestAny, nil
}

func (s *Scheduler) pickWeightedRandom(candidates []WorkerCandidate, activeOnly bool) (WorkerCandidate, bool) {
	type weightedCandidate struct {
		idx    int
		weight float64
	}

	weighted := make([]weightedCandidate, 0, len(candidates))
	totalWeight := 0.0

	for i, candidate := range candidates {
		if candidate.Info == nil {
			continue
		}
		if activeOnly && !isActiveWorker(candidate) {
			continue
		}

		weight := s.WorkerWeight(candidate.Info)
		if math.IsNaN(weight) || weight < 0 {
			weight = 0
		}

		weighted = append(weighted, weightedCandidate{idx: i, weight: weight})
		totalWeight += weight
	}

	if len(weighted) == 0 {
		return WorkerCandidate{}, false
	}

	if totalWeight <= 0 {
		return s.pickLeastUsed(candidates, activeOnly)
	}

	roll := s.rng.Float64() * totalWeight
	for _, candidate := range weighted {
		roll -= candidate.weight
		if roll <= 0 {
			return candidates[candidate.idx], true
		}
	}

	return candidates[weighted[len(weighted)-1].idx], true
}

func (s *Scheduler) pickLeastUsed(candidates []WorkerCandidate, activeOnly bool) (WorkerCandidate, bool) {
	bestScore := math.MaxFloat64
	bestIdx := -1

	for i, candidate := range candidates {
		if candidate.Info == nil {
			continue
		}
		if activeOnly && !isActiveWorker(candidate) {
			continue
		}

		score := s.WorkerLoadScore(candidate.Info)
		if score < bestScore {
			bestScore = score
			bestIdx = i
		}
	}

	if bestIdx == -1 {
		return WorkerCandidate{}, false
	}

	return candidates[bestIdx], true
}

func (s *Scheduler) WorkerLoadScore(info *workerv1.GetWorkerInfoResponse) float64 {
	if info == nil {
		return math.MaxFloat64
	}

	cpuRatio := usageRatio(info.GetCpuUsage(), cpuUsageScale)
	memRatio := usageRatio(info.GetMemUsage(), info.GetMemBudget())

	// CPU tends to be the main bottleneck for sandbox workloads.
	return (0.7 * cpuRatio) + (0.3 * memRatio)
}

func (s *Scheduler) WorkerWeight(info *workerv1.GetWorkerInfoResponse) float64 {
	if info == nil {
		return 0
	}

	cpuLeft := leftRatio(info.GetCpuUsage(), cpuUsageScale)
	memLeft := leftRatio(info.GetMemUsage(), info.GetMemBudget())

	return (0.7 * cpuLeft) + (0.3 * memLeft)
}

// hasCapacity reports whether a worker still has both CPU and memory budget
// left; either resource being exhausted makes it unable to host a new sandbox.
func hasCapacity(info *workerv1.GetWorkerInfoResponse) bool {
	return usageRatio(info.GetCpuUsage(), cpuUsageScale) < 1 &&
		usageRatio(info.GetMemUsage(), info.GetMemBudget()) < 1
}

func usageRatio(usage, budget int32) float64 {
	if usage <= 0 {
		return 0
	}
	if budget <= 0 {
		return float64(usage) / 100.0
	}

	ratio := float64(usage) / float64(budget)
	if ratio < 0 {
		return 0
	}

	return ratio
}

func leftRatio(usage, budget int32) float64 {
	ratio := usageRatio(usage, budget)
	left := 1 - ratio
	if left < 0 {
		return 0
	}
	if left > 1 {
		return 1
	}
	return left
}

func isActiveWorker(candidate WorkerCandidate) bool {
	workerStatus := strings.ToLower(strings.TrimSpace(string(candidate.Worker.Status)))
	infoStatus := strings.ToLower(strings.TrimSpace(candidate.Info.GetStatus()))

	if workerStatus == "" && infoStatus == "" {
		return true
	}

	if workerStatus != "" && workerStatus != "active" {
		return false
	}

	if infoStatus != "" && infoStatus != "active" {
		return false
	}

	return true
}
