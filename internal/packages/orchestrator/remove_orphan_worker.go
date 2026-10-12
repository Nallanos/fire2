package orchestrator

import (
	"context"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// RemoveOrphanArgs asks a worker to remove a sandbox it reported in its
// heartbeat inventory but that the database no longer attributes to it
// (unknown, terminal, or assigned elsewhere).
type RemoveOrphanArgs struct {
	WorkerID  string `json:"worker_id"`
	SandboxID string `json:"sandbox_id"`
}

func (RemoveOrphanArgs) Kind() string { return "remove_orphan_sandbox" }

// InsertOpts dedups against jobs still waiting or running: every heartbeat
// lists the orphan again until the worker has actually removed it.
func (RemoveOrphanArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: "cleanup",
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
}

type RemoveOrphanWorker struct {
	river.WorkerDefaults[RemoveOrphanArgs]
	workerRepo workerpkg.Repository
}

func NewRemoveOrphanWorker(workerRepo workerpkg.Repository) *RemoveOrphanWorker {
	return &RemoveOrphanWorker{workerRepo: workerRepo}
}

func (w *RemoveOrphanWorker) Work(ctx context.Context, job *river.Job[RemoveOrphanArgs]) error {
	return removeSandboxOnWorker(ctx, w.workerRepo, job.Args.WorkerID, job.Args.SandboxID)
}
