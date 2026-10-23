// Package predeploy runs the pre-deployment command for a generation before
// that generation is handed to the deployer.
package predeploy

import (
	"context"
	"sync"
	"time"

	pb "github.com/nlewo/comin/internal/protobuf"

	"github.com/sirupsen/logrus"
)

// SubmitFunc is called when the command exited 0 for g and no newer generation is waiting.
type SubmitFunc func(g *pb.Generation, operation string)

// FailFunc is called when the command failed for g and no newer generation is waiting.
type FailFunc func(g *pb.Generation, operation string, err error)

// Entry is a generation held by the stage. StartedAt is zero while pending.
type Entry struct {
	Generation *pb.Generation
	Operation  string
	StartedAt  time.Time
}

// Status is a snapshot of the stage; nil fields mean nothing running or pending.
type Status struct {
	Running *Entry
	Pending *Entry
}

// Stage runs the command for one generation at a time. A generation offered
// while the command runs replaces any previously pending one; the running
// command is never signalled because of an Offer.
type Stage struct {
	command string
	submit  SubmitFunc
	fail    FailFunc
	wake    chan struct{}

	mu      sync.Mutex
	ctx     context.Context
	running *Entry
	pending *Entry
}

// New returns a stage that runs command, which must be non-empty. Call Run before relying on Offer.
func New(command string, submit SubmitFunc, fail FailFunc) *Stage {
	return &Stage{
		command: command,
		submit:  submit,
		fail:    fail,
		wake:    make(chan struct{}, 1),
	}
}

// Run starts the worker and returns; call it once. Once ctx is done the stage
// starts no command and calls neither submit nor fail; Offer then does nothing.
func (s *Stage) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	go s.work(ctx)
}

// Offer never blocks. Without a running command it starts one for g at once.
// submit and fail run on the stage's goroutine, which holds Running until they return.
func (s *Stage) Offer(g *pb.Generation, operation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil && s.ctx.Err() != nil {
		return
	}
	if s.running != nil {
		s.pending = &Entry{Generation: g, Operation: operation}
		return
	}
	s.running = &Entry{Generation: g, Operation: operation, StartedAt: time.Now()}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status returns a snapshot, safe from any goroutine.
func (s *Stage) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Running: copyEntry(s.running), Pending: copyEntry(s.pending)}
}

func copyEntry(e *Entry) *Entry {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

func (s *Stage) work(ctx context.Context) {
	defer s.clear()
	for {
		cur := s.current()
		if cur == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		logrus.Infof("pre-deployment command started for generation %s", cur.Generation.Uuid)
		err := runHook(ctx, s.command, cur.Generation, cur.Operation)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logrus.Errorf("pre-deployment command failed for generation %s: %s", cur.Generation.Uuid, err)
		}
		if s.promotePending() {
			logrus.Infof("pre-deployment command finished for superseded generation %s", cur.Generation.Uuid)
			continue
		}
		if err != nil {
			s.fail(cur.Generation, cur.Operation, err)
		} else {
			s.submit(cur.Generation, cur.Operation)
		}
		s.finish()
	}
}

func (s *Stage) current() *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// promotePending makes the pending entry the running one, if any.
func (s *Stage) promotePending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promoteLocked()
}

func (s *Stage) promoteLocked() bool {
	if s.pending == nil {
		return false
	}
	s.running, s.pending = s.pending, nil
	s.running.StartedAt = time.Now()
	return true
}

// finish releases Running after submit or fail returned, or starts a generation offered meanwhile.
func (s *Stage) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = nil
	s.promoteLocked()
}

func (s *Stage) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.pending = nil, nil
}
