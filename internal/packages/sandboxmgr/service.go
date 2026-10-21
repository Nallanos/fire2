package sandboxmgr

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// statePollInterval is how often a Service samples its VM's state. Firecracker
// has no push-based event stream; only the process exit (Wait) is pushed.
const statePollInterval = 2 * time.Second

// stateQueryTimeout bounds one DescribeInstanceInfo call.
const stateQueryTimeout = time.Second

type commandKind int

const (
	cmdCreate commandKind = iota
	cmdStop
	cmdRemove
)

type command struct {
	kind  commandKind
	in    CreateInput
	reply chan error // buffered (1): the actor never blocks on a reply
}

// Service is the actor of one sandbox. Its run goroutine is the only code
// touching the sandbox's VM: commands arrive on cmds and are applied one at a
// time, so no lock guards the fields below the line.
type Service struct {
	id           string
	log          *slog.Logger // carries sandbox_id
	newVM        vmFactory
	report       func(State) // called only when the state changes
	cmds         chan command
	done         chan struct{} // closed when run returns, panic included
	pollInterval time.Duration

	// Owned by run.
	vm     vm
	state  State
	exited chan error // nil until the VM boots: a nil channel never fires in a select
}

func newService(id string, newVM vmFactory, report func(State), pollInterval time.Duration) *Service {
	return &Service{
		id:           id,
		log:          slog.With("sandbox_id", id),
		newVM:        newVM,
		report:       report,
		cmds:         make(chan command),
		done:         make(chan struct{}),
		pollInterval: pollInterval,
		state:        StateStarting,
	}
}

// do hands cmd to the actor and waits for its reply. It does not take a ctx:
// the actor's work cannot be interrupted halfway, and the Manager needs the
// outcome to keep its map consistent.
func (s *Service) do(cmd command) error {
	cmd.reply = make(chan error, 1)
	select {
	case s.cmds <- cmd:
	case <-s.done:
		return errServiceExited
	}
	// A command that ends the actor (remove, failed create) replies before
	// done is closed, but select picks at random among ready cases: check
	// reply again before giving up.
	select {
	case err := <-cmd.reply:
		return err
	case <-s.done:
		select {
		case err := <-cmd.reply:
			return err
		default:
			return errServiceExited
		}
	}
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("service panicked", "panic", r)
			s.setState(StateDead)
		}
	}()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case cmd := <-s.cmds:
			exit, err := s.handle(ctx, cmd)
			cmd.reply <- err
			if exit {
				return
			}

		case err := <-s.exited:
			s.exited = nil
			s.log.Warn("firecracker exited", "err", err)
			s.setState(StateDead)

		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

// handle applies one command. exit reports whether the actor is done: after a
// remove, or a create that failed (and cleaned up after itself).
func (s *Service) handle(ctx context.Context, cmd command) (exit bool, err error) {
	switch cmd.kind {
	case cmdCreate:
		if err := s.create(ctx, cmd.in); err != nil {
			return true, err
		}
		return false, nil

	case cmdStop:
		if s.vm == nil {
			return false, nil
		}
		if err := s.vm.Stop(); err != nil {
			return false, fmt.Errorf("stop sandbox %s: %w", s.id, err)
		}
		return false, nil

	case cmdRemove:
		return true, s.remove()

	default:
		return false, fmt.Errorf("sandbox %s: unknown command %d", s.id, cmd.kind)
	}
}

func (s *Service) create(ctx context.Context, in CreateInput) error {
	if s.vm != nil {
		return fmt.Errorf("sandbox %s: already created", s.id)
	}
	v, err := s.newVM(in)
	if err != nil {
		return err
	}
	s.vm = v
	if err := v.Create(ctx); err != nil {
		if cleanupErr := s.remove(); cleanupErr != nil {
			s.log.Error("cleanup after failed create failed", "err", cleanupErr)
		}
		return err
	}

	s.watch(ctx, v)

	// Report RUNNING now rather than one poll interval later.
	s.poll(ctx)
	return nil
}

// adopt hands the Service a VM a previous worker process left running (its
// shim survived), in the state it was found in. Call it before run: there is
// nothing to create.
func (s *Service) adopt(ctx context.Context, v vm, st State) {
	s.vm = v
	s.state = st
	s.watch(ctx, v)
}

// watch turns the VM's exit into a value on s.exited, which run turns into
// DEAD.
func (s *Service) watch(ctx context.Context, v vm) {
	// The goroutine captures the channel, not the field: run sets s.exited
	// to nil once it fires, and reading the field here would race with that.
	exited := make(chan error, 1)
	s.exited = exited
	go func() { exited <- v.Wait(ctx) }()
}

// remove stops the VM and releases its resources. A stop failure doesn't
// block the cleanup: leaving the volume and directory behind forever is worse
// than a best-effort cleanup that reports the error.
func (s *Service) remove() error {
	if s.vm == nil {
		return nil
	}
	stopErr := s.vm.Stop()
	if stopErr != nil {
		s.log.Warn("stop failed, cleaning up anyway", "err", stopErr)
	}
	if err := s.vm.Cleanup(); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", s.id, err)
	}
	if stopErr != nil {
		return fmt.Errorf("stop sandbox %s (cleaned up): %w", s.id, stopErr)
	}
	return nil
}

// poll samples the VM's state. DEAD is final: only Wait can set it, and once
// the process is gone there is nothing left to query.
func (s *Service) poll(ctx context.Context) {
	if s.vm == nil || s.state == StateDead {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, stateQueryTimeout)
	defer cancel()
	st, err := s.vm.State(qctx)
	if err != nil {
		st = StateUnknown
	}
	s.setState(st)
}

func (s *Service) setState(st State) {
	if st == s.state {
		return
	}
	s.state = st
	s.report(st)
}
