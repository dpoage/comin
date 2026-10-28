// Package reconcile decides the next step that moves the host toward the
// preferred admitted candidate. Decide is a pure function: it reads one
// Observation and keeps nothing between calls.
package reconcile

import (
	"time"

	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
)

// Trigger values of the COMIN_TRIGGER ask environment: a pass started by
// switch-latest asks with manual, every other pass with poll.
const (
	TriggerPoll   = "poll"
	TriggerManual = "manual"
)

// State values of Status.State.
const (
	StateConverged = "converged"
	StateWaiting   = "waiting"
	StateDeploying = "deploying"
	StateFailed    = "failed"
)

// ReasonNoCandidate is the reason of a walk over no candidate.
const ReasonNoCandidate = "no candidate"

// Stage is how far the generation of a candidate has progressed.
type Stage int

const (
	// NoGeneration: none evaluated, or a failed one whose retry is due.
	NoGeneration Stage = iota
	Evaluating
	// AwaitingBuild: evaluated, build not started.
	AwaitingBuild
	Building
	Built
	EvalFailed
	BuildFailed
	MachineIdMismatch
)

// Candidate is a repository candidate with the facts observed about its
// generation. Verdict is the answer of the ask made for it in this pass.
type Candidate struct {
	repository.Candidate
	Stage          Stage
	Err            string
	GenerationUuid string
	OutPath        string
	// Realized: OutPath is in the Nix store. Read for Built candidates only.
	Realized bool
	// BuildSubmitted: the generation already waits on the build confirmer.
	BuildSubmitted bool
	Verdict        *preflight.Verdict
}

// DeployConfirm is the state of the deploy confirmer, by generation uuid.
type DeployConfirm struct {
	// Pending: submitted and not yet confirmed.
	Pending string
	// Confirmed: confirmed and not yet handed to the deployer.
	Confirmed string
}

// Target is the candidate a Status row names: Commit, its real Branch name
// and OutPath, empty when it has none.
type Target struct {
	Commit  string
	Branch  string
	OutPath string
}

// Status labels the outcome of a pass: State is one of the State values,
// Desired is nil unless the row names a candidate, and Reason is the
// comin-authored or verbatim preflight line of the row. Candidates is the
// walk the pass ran, in preference order.
type Status struct {
	State      string
	Desired    *Target
	Reason     string
	Candidates []CandidateStatus
}

// CandidateStatus is one entry of the walk a status reports.
type CandidateStatus struct {
	Branch  string
	Kind    string
	Commit  string
	OutPath string
	Outcome string
	Reason  string
}

// Outcome values of CandidateStatus.Outcome.
const (
	OutcomeAdmitted = "admitted"
	OutcomeRefused  = "refused"
	OutcomeFailed   = "failed"
	OutcomePending  = "pending"
	OutcomeSkipped  = "skipped"
)

// TargetOf returns the status target of a candidate.
func TargetOf(c Candidate) Target {
	return Target{Commit: c.CommitId, Branch: c.Branch, OutPath: c.OutPath}
}

// Observation is everything one pass knows. Candidates are in preference order.
type Observation struct {
	Candidates []Candidate
	// Current is the running system; "" when unknown, with CurrentErr its cause.
	Current    string
	CurrentErr string
	// Deployments are the stored deployments in any order.
	Deployments []*protobuf.Deployment
	// InFlight: a deployment is submitted or running. InFlightTarget is
	// its candidate; nil when InFlight is false.
	InFlight       bool
	InFlightTarget *Target
	Suspended      bool
	// ProcessStartedAt is when this comin process started: a finished
	// failure older than it is retried once.
	ProcessStartedAt time.Time
	// AsksRun: a preflight command is configured, so an ask reads the store path.
	AsksRun       bool
	DeployConfirm DeployConfirm
	Trigger       string
}

type Action int

const (
	// Wait: nothing to do this pass.
	Wait Action = iota
	// Eval: evaluate Target (never evaluated, a failure due its retry, or its out path vanished).
	Eval
	// SubmitBuild: submit Target's generation to the build confirmer.
	SubmitBuild
	// Ask: ask the preflight about Target and decide again.
	Ask
	// SubmitDeploy: submit Target's generation to the deploy confirmer.
	SubmitDeploy
	// Deploy: hand Target's generation to the deployer.
	Deploy
)

// Decision is the next step. Target indexes Observation.Candidates;
// Target is -1 for Wait.
type Decision struct {
	Action Action
	Target int
	// Status labels the pass this decision ends.
	Status Status
	// Decisive reports whether this decision ends a pending manual
	// request: a deployment was handed over, or the walk concluded while
	// no deployment is in flight.
	Decisive bool
}

// Decide walks the candidates in order, passing failed and refused ones and
// stopping at the first pending or admitted one. Nothing below the stop
// point is evaluated, built or asked. An admitted candidate is deployed iff
// no deployment is in flight, the running system is known and differs from
// its out path, and failedLatest does not block its out path; a manual
// pass retries a blocked out path once.
func Decide(o Observation) Decision {
	d := decide(o)
	if o.InFlight {
		// A deployment in flight is the first status row, whatever the
		// walk is preparing next, and a request made meanwhile waits for
		// the pass after it.
		d.Status.State = StateDeploying
		d.Status.Desired = o.InFlightTarget
		d.Status.Reason = ""
		d.Decisive = false
	}
	return d
}

func decide(o Observation) Decision {
	if o.Suspended {
		return waiting(Status{State: StateWaiting, Reason: "suspended"})
	}
	var firstFailed, firstRefused *Candidate
	var entries []CandidateStatus
	for i := range o.Candidates {
		c := &o.Candidates[i]
		switch outcome(*c, o.AsksRun) {
		case failed:
			if firstFailed == nil {
				firstFailed = c
			}
			entries = append(entries, entry(*c, OutcomeFailed, failedReason(*c)))
		case refused:
			if firstRefused == nil {
				firstRefused = c
			}
			entries = append(entries, entry(*c, OutcomeRefused, c.Verdict.Reason))
		case needsEval, needsBuildSubmit, pending:
			st := pendingStatus(*c)
			st.Candidates = appendStopped(entries, entry(*c, OutcomePending, st.Reason), o.Candidates[i+1:])
			switch outcome(*c, o.AsksRun) {
			case needsEval:
				return Decision{Action: Eval, Target: i, Status: st}
			case needsBuildSubmit:
				return Decision{Action: SubmitBuild, Target: i, Status: st}
			}
			return waiting(st)
		case unasked:
			if o.InFlight {
				// Asks wait for the deployment in flight to end.
				st := Status{State: StateWaiting}
				st.Candidates = appendStopped(entries, entry(*c, OutcomePending, inFlightReason(o)), o.Candidates[i+1:])
				return waiting(st)
			}
			return Decision{Action: Ask, Target: i}
		case admitted:
			d := deploy(o, i)
			if d.Action == Eval {
				// Its out path vanished: the candidate is evaluated
				// again before its verdict is used.
				st := pendingStatus(*c)
				st.Candidates = appendStopped(entries, entry(*c, OutcomePending, st.Reason), o.Candidates[i+1:])
				d.Status = st
				return d
			}
			entries = appendStopped(entries, entry(*c, OutcomeAdmitted, ""), o.Candidates[i+1:])
			d.Status.Candidates = entries
			return d
		}
	}
	switch {
	case firstFailed != nil:
		return Decision{Action: Wait, Target: -1, Status: withCandidates(failedStatus(*firstFailed), entries), Decisive: true}
	case firstRefused != nil:
		return Decision{Action: Wait, Target: -1, Status: withCandidates(Status{State: StateWaiting, Reason: firstRefused.Verdict.Reason}, entries), Decisive: true}
	}
	return Decision{Action: Wait, Target: -1, Status: withCandidates(Status{State: StateWaiting, Reason: ReasonNoCandidate}, entries), Decisive: true}
}

// entry builds the walk entry of c.
func entry(c Candidate, result, reason string) CandidateStatus {
	kind := "main"
	if c.Testing {
		kind = "testing"
	}
	return CandidateStatus{Branch: c.Branch, Kind: kind, Commit: c.CommitId, OutPath: c.OutPath, Outcome: result, Reason: reason}
}

// appendStopped ends a walk stopped at stop: everything below is skipped.
func appendStopped(entries []CandidateStatus, stop CandidateStatus, rest []Candidate) []CandidateStatus {
	entries = append(entries, stop)
	for _, c := range rest {
		entries = append(entries, entry(c, OutcomeSkipped, ""))
	}
	return entries
}

func withCandidates(st Status, entries []CandidateStatus) Status {
	st.Candidates = entries
	return st
}

func waiting(st Status) Decision {
	return Decision{Action: Wait, Target: -1, Status: st}
}

// pendingStatus labels a walk stopped at a candidate that cannot be asked
// yet, or that is evaluated again after a retry is due or its out path vanished.
func pendingStatus(c Candidate) Status {
	return Status{State: StateWaiting, Reason: pendingReason(c)}
}

func pendingReason(c Candidate) string {
	switch c.Stage {
	case AwaitingBuild:
		return "awaiting build confirmation " + c.CommitId
	case Building:
		return "building " + c.CommitId
	}
	return "evaluating " + c.CommitId
}

// inFlightReason is the reason of a candidate the walk does not ask while
// a deployment is in flight: the commit of the in-flight target, when known.
func inFlightReason(o Observation) string {
	if o.InFlightTarget == nil {
		return "deployment in flight"
	}
	return "deployment in flight " + o.InFlightTarget.Commit
}

// failedStatus labels the first failed candidate of an exhausted walk.
func failedStatus(c Candidate) Status {
	t := TargetOf(c)
	return Status{State: StateFailed, Desired: &t, Reason: failedReason(c)}
}

func failedReason(c Candidate) string {
	switch c.Stage {
	case EvalFailed:
		return withErr("eval failed "+c.CommitId, c.Err)
	case BuildFailed:
		return withErr("build failed "+c.CommitId, c.Err)
	case MachineIdMismatch:
		return "machine-id mismatch " + c.CommitId
	}
	return "unsigned commit " + c.CommitId
}

func withErr(s, err string) string {
	if err == "" {
		return s
	}
	return s + ": " + err
}

func deploy(o Observation, i int) Decision {
	d := o.Candidates[i]
	t := TargetOf(d)
	last, blocked := failedLatest(o.Deployments, d.OutPath, o.ProcessStartedAt)
	// A manual pass retries a blocked out path once; poll passes never do.
	blocked = blocked && o.Trigger != TriggerManual
	switch {
	case o.InFlight:
		return Decision{Action: Wait, Target: -1}
	case o.Current == "":
		return waiting(Status{State: StateWaiting, Desired: &t, Reason: withErr("current system unknown", o.CurrentErr)})
	case o.Current == d.OutPath:
		return Decision{Action: Wait, Target: -1, Status: Status{State: StateConverged, Desired: &t}, Decisive: true}
	case blocked:
		return Decision{Action: Wait, Target: -1,
			Status:   Status{State: StateFailed, Desired: &t, Reason: withErr("deploy failed "+d.CommitId, last.ErrorMsg)},
			Decisive: true}
	case !d.Realized:
		return Decision{Action: Eval, Target: i, Status: pendingStatus(d)}
	case o.DeployConfirm.Pending == d.GenerationUuid:
		return waiting(Status{State: StateWaiting, Desired: &t, Reason: "awaiting deploy confirmation " + d.CommitId})
	}
	st := Status{State: StateDeploying, Desired: &t}
	if o.DeployConfirm.Confirmed == d.GenerationUuid {
		return Decision{Action: Deploy, Target: i, Status: st, Decisive: true}
	}
	return Decision{Action: SubmitDeploy, Target: i, Status: st}
}

type result int

const (
	failed result = iota
	refused
	pending
	needsEval
	needsBuildSubmit
	unasked
	admitted
)

// outcome classifies a candidate. A Built candidate whose out path vanished
// is evaluated again before any ask that reads it.
func outcome(c Candidate, asksRun bool) result {
	if !c.Verified {
		return failed
	}
	switch c.Stage {
	case NoGeneration:
		return needsEval
	case Evaluating, Building:
		return pending
	case AwaitingBuild:
		if c.BuildSubmitted {
			return pending
		}
		return needsBuildSubmit
	case EvalFailed, BuildFailed, MachineIdMismatch:
		return failed
	}
	if asksRun && !c.Realized {
		return needsEval
	}
	switch {
	case c.Verdict == nil:
		return unasked
	case c.Verdict.Admitted:
		return admitted
	}
	return refused
}

// failedLatest returns the last finished deployment of outPath and whether
// it blocks outPath: it failed in this process, or the finished one before
// it failed too, so a lone pre-start failure is retried once.
func failedLatest(deployments []*protobuf.Deployment, outPath string, startedAt time.Time) (last *protobuf.Deployment, blocked bool) {
	var prev *protobuf.Deployment
	for _, d := range deployments {
		if d.Generation.GetOutPath() != outPath {
			continue
		}
		if d.Status != store.StatusToString(store.Done) && d.Status != store.StatusToString(store.Failed) {
			continue
		}
		// The store keeps a row per deployment twice (the row the deployer
		// transitions and the copy the manager commits); one deployment
		// counts once.
		if last != nil && d.GetUuid() == last.GetUuid() && d.GetEndedAt().AsTime().Equal(last.GetEndedAt().AsTime()) {
			continue
		}
		if last == nil || d.EndedAt.AsTime().After(last.EndedAt.AsTime()) {
			prev, last = last, d
		} else if prev == nil || d.EndedAt.AsTime().After(prev.EndedAt.AsTime()) {
			prev = d
		}
	}
	if last == nil || last.Status != store.StatusToString(store.Failed) {
		return last, false
	}
	return last, !last.EndedAt.AsTime().Before(startedAt) || (prev != nil && prev.Status == store.StatusToString(store.Failed))
}
