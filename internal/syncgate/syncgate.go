// Package syncgate asks an external command whether comin may fetch,
// evaluate, build or deploy now.
package syncgate

import (
	"context"
	"time"

	"github.com/nlewo/comin/internal/preflight"
)

// WatchPeriod is the period of the gate checks while an evaluation or a
// build runs.
const WatchPeriod = 2 * time.Second

// DefaultTimeoutSeconds is the timeout of a gate run unless configured.
const DefaultTimeoutSeconds = 30

// Stage is the point of comin the gate is asked from.
type Stage string

const (
	// StageFetch is asked before every fetch start.
	StageFetch Stage = "fetch"
	// StagePass is asked once at the start of every reconcile pass.
	StagePass Stage = "pass"
	// StageWatch is asked every WatchPeriod while an evaluation or a build runs.
	StageWatch Stage = "watch"
)

// Trigger is what the asked work serves.
type Trigger string

const (
	Poll   Trigger = "poll"
	Manual Trigger = "manual"
)

// Command is the configured gate. Path "" means no gate: it is always open.
type Command struct {
	Path    string
	Timeout time.Duration
}

// Answer is the outcome of Ask. Reason is set when the gate is closed.
type Answer struct {
	Open   bool
	Reason string
}

// Configured reports whether a gate command is set.
func (c Command) Configured() bool {
	return c.Path != ""
}

// Ask runs the command with COMIN_TRIGGER and COMIN_SYNC_STAGE added to
// comin's environment. Exit 0 within the timeout is open; any other
// outcome is closed, and its reason is the first stdout line, else a line
// naming the failure. Timeout <= 0 with a Path panics.
func (c Command) Ask(ctx context.Context, stage Stage, trigger Trigger) Answer {
	if !c.Configured() {
		return Answer{Open: true}
	}
	v := preflight.Ask(ctx, preflight.Command{Path: c.Path, Timeout: c.Timeout}, []string{
		"COMIN_TRIGGER=" + string(trigger),
		"COMIN_SYNC_STAGE=" + string(stage),
	})
	if v.Admitted {
		return Answer{Open: true}
	}
	return Answer{Reason: v.Reason}
}
