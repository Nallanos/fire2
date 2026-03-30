package orchestrator

import (
	"errors"
	"math"
	"strings"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
)

var ErrNoWorkerCandidates = errors.New("no worker candidates available")

type WorkerCandidate struct {
	Worker db.Worker
	Info   *workerv1.GetWorkerInfoResponse
}

type Scheduler struct{}

func NewScheduler() *Scheduler {
	return &Scheduler{}
}

func (s *Scheduler) ChooseLeastUsedWorker(candidates []WorkerCandidate) (WorkerCandidate, error) {
	if len(candidates) == 0 {
		return WorkerCandidate{}, ErrNoWorkerCandidates
	}

	bestActive, hasActive := s.pickLeastUsed(candidates, true)
	if hasActive {
		return bestActive, nil
	}

	bestAny, ok := s.pickLeastUsed(candidates, false)
	if !ok {
		return WorkerCandidate{}, ErrNoWorkerCandidates
	}

	return bestAny, nil
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

	cpuRatio := usageRatio(info.GetCpuUsage(), info.GetCpuBudget())
	memRatio := usageRatio(info.GetMemUsage(), info.GetMemBudget())

	// CPU tends to be the main bottleneck for sandbox workloads.
	return (0.7 * cpuRatio) + (0.3 * memRatio)
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

func isActiveWorker(candidate WorkerCandidate) bool {
	workerStatus := strings.ToLower(strings.TrimSpace(candidate.Worker.Status))
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
