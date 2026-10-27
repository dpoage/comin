// Package reconcile decides the next step that moves the host toward the
// preferred admitted candidate. Decide is a pure function: it reads one
// Observation and keeps nothing between calls.
package reconcile

import (
	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
)

// Stage is how far the generation of a candidate has progressed.
type Stage int

const (
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

// Activation is the answer of the last ask made right before an activation.
type Activation struct {
	OutPath string
	Verdict preflight.Verdict
}

// Observation is everything one pass knows. Candidates are in preference order.
type Observation struct {
	Candidates []Candidate
	// Current is the running system; "" when unknown, with CurrentErr its cause.
	Current    string
	CurrentErr string
	// Deployments are the stored deployments in any order.
	Deployments []*protobuf.Deployment
	// InFlight: a deployment is submitted or running.
	InFlight  bool
	Suspended bool
	// AsksRun: a preflight command is configured, so an ask reads the store path.
	AsksRun        bool
	DeployConfirm  DeployConfirm
	Trigger        string
	LastActivation *Activation
}

type Action int

const (
	// Wait: nothing to do this pass.
	Wait Action = iota
	// Eval: evaluate Target again (never evaluated, or its out path vanished).
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

// Decision is the next step. Target and Desired index Observation.Candidates;
// Target is -1 for Wait, Desired is -1 when no candidate is admitted.
type Decision struct {
	Action  Action
	Target  int
	Desired int
}

func wait(desired int) Decision { return Decision{Action: Wait, Target: -1, Desired: desired} }

// Decide walks the candidates in order, passing failed and refused ones and
// stopping at the first pending or admitted one. Nothing below the stop
// point is evaluated, built or asked. An admitted candidate is deployed iff
// no deployment is in flight, the running system is known and differs from
// its out path, and its latest finished deployment did not fail.
func Decide(o Observation) Decision {
	if o.Suspended {
		return wait(-1)
	}
	for i, c := range o.Candidates {
		switch outcome(c, o.AsksRun) {
		case failed, refused:
			continue
		case needsEval:
			return Decision{Action: Eval, Target: i, Desired: -1}
		case needsBuildSubmit:
			return Decision{Action: SubmitBuild, Target: i, Desired: -1}
		case pending:
			return wait(-1)
		case unasked:
			if o.InFlight {
				return wait(-1)
			}
			return Decision{Action: Ask, Target: i, Desired: -1}
		case admitted:
			return deploy(o, i)
		}
	}
	return wait(-1)
}

func deploy(o Observation, i int) Decision {
	d := o.Candidates[i]
	switch {
	case o.InFlight, o.Current == "", o.Current == d.OutPath, failedLatest(o.Deployments, d.OutPath):
		return wait(i)
	case !d.Realized:
		return Decision{Action: Eval, Target: i, Desired: -1}
	case o.DeployConfirm.Confirmed == d.GenerationUuid:
		return Decision{Action: Deploy, Target: i, Desired: i}
	case o.DeployConfirm.Pending == d.GenerationUuid:
		return wait(i)
	}
	return Decision{Action: SubmitDeploy, Target: i, Desired: i}
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

// failedLatest reports whether, among the deployments of outPath that
// finished, the one that ended last failed.
func failedLatest(deployments []*protobuf.Deployment, outPath string) bool {
	var last *protobuf.Deployment
	for _, d := range deployments {
		if d.Generation.GetOutPath() != outPath {
			continue
		}
		if d.Status != store.StatusToString(store.Done) && d.Status != store.StatusToString(store.Failed) {
			continue
		}
		if last == nil || d.EndedAt.AsTime().After(last.EndedAt.AsTime()) {
			last = d
		}
	}
	return last != nil && last.Status == store.StatusToString(store.Failed)
}
