package reconcile

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	admit  = &preflight.Verdict{Admitted: true}
	refuse = &preflight.Verdict{Reason: "refused: gate"}
)

func cand(name string, stage Stage, verdict *preflight.Verdict) Candidate {
	return Candidate{
		Candidate:      repository.Candidate{CommitId: name, Branch: "b-" + name, Verified: true},
		Stage:          stage,
		GenerationUuid: "uuid-" + name,
		OutPath:        "/out/" + name,
		Realized:       stage == Built,
		Verdict:        verdict,
	}
}

func obs(current string, cs ...Candidate) Observation {
	return Observation{Candidates: cs, Current: current, AsksRun: true}
}

// walkEntry is the status entry expected for cand(name).
func walkEntry(name, result, reason string) CandidateStatus {
	return CandidateStatus{Branch: "b-" + name, Kind: "main", Commit: name, OutPath: "/out/" + name, Outcome: result, Reason: reason}
}

func deployment(out, status string, endedAt int64) *protobuf.Deployment {
	return &protobuf.Deployment{
		Generation: &protobuf.Generation{OutPath: out},
		Status:     status,
		EndedAt:    timestamppb.New(time.Unix(endedAt, 0)),
	}
}

// assertDecision compares the action and its labels, not the walk entries
// reported in Status.Candidates (owned by the status tests).
func assertDecision(t *testing.T, want, got Decision) {
	t.Helper()
	assert.Equal(t, want.Action, got.Action)
	assert.Equal(t, want.Target, got.Target)
	assert.Equal(t, want.Desired, got.Desired)
	assert.Equal(t, want.Decisive, got.Decisive)
	assert.Equal(t, want.Status.State, got.Status.State)
	assert.Equal(t, want.Status.Desired, got.Status.Desired)
	assert.Equal(t, want.Status.Reason, got.Status.Reason)
}

func TestDecideWalk(t *testing.T) {
	unsigned := cand("t", Built, nil)
	unsigned.Verified = false
	tests := []struct {
		name string
		o    Observation
		want Decision
	}{
		{"no candidate", obs("/out/x"),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateWaiting, Reason: "no candidate"}, Decisive: true}},
		{"never evaluated", obs("/out/x", cand("t", NoGeneration, nil), cand("m", NoGeneration, nil)),
			Decision{Action: Eval, Target: 0, Desired: -1, Status: Status{State: StateWaiting, Reason: "evaluating t"}}},
		{"evaluating stops the walk", obs("/out/x", cand("t", Evaluating, nil), cand("m", NoGeneration, nil)),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateWaiting, Reason: "evaluating t"}}},
		{"building stops the walk", obs("/out/x", cand("t", Building, nil), cand("m", NoGeneration, nil)),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateWaiting, Reason: "building t"}}},
		{"build to confirm", obs("/out/x", cand("t", AwaitingBuild, nil), cand("m", NoGeneration, nil)),
			Decision{Action: SubmitBuild, Target: 0, Desired: -1, Status: Status{State: StateWaiting, Reason: "awaiting build confirmation t"}}},
		{"build confirmation pending stops the walk", obs("/out/x", func() Candidate { c := cand("t", AwaitingBuild, nil); c.BuildSubmitted = true; return c }(), cand("m", NoGeneration, nil)),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateWaiting, Reason: "awaiting build confirmation t"}}},
		{"unasked is asked before anything below", obs("/out/x", cand("t", Built, nil), cand("m", NoGeneration, nil)),
			Decision{Action: Ask, Target: 0, Desired: -1}},
		{"refused passes to the next", obs("/out/x", cand("t", Built, refuse), cand("m", NoGeneration, nil)),
			Decision{Action: Eval, Target: 1, Desired: -1, Status: Status{State: StateWaiting, Reason: "evaluating m"}}},
		{"refused then unasked", obs("/out/x", cand("t", Built, refuse), cand("m", Built, nil)),
			Decision{Action: Ask, Target: 1, Desired: -1}},
		{"eval failed passes", obs("/out/x", cand("t", EvalFailed, nil), cand("m", NoGeneration, nil)),
			Decision{Action: Eval, Target: 1, Desired: -1, Status: Status{State: StateWaiting, Reason: "evaluating m"}}},
		{"build failed passes", obs("/out/x", cand("t", BuildFailed, nil), cand("m", Built, nil)),
			Decision{Action: Ask, Target: 1, Desired: -1}},
		{"machine-id mismatch passes", obs("/out/x", cand("t", MachineIdMismatch, nil), cand("m", Built, nil)),
			Decision{Action: Ask, Target: 1, Desired: -1}},
		{"unsigned passes without evaluating", obs("/out/x", unsigned, cand("m", Built, nil)),
			Decision{Action: Ask, Target: 1, Desired: -1}},
		{"all failed or refused", obs("/out/x", cand("t", Built, refuse), cand("m", BuildFailed, nil)),
			Decision{Action: Wait, Target: -1, Desired: -1,
				Status:   Status{State: StateFailed, Desired: &Target{Commit: "m", Branch: "b-m", OutPath: "/out/m"}, Reason: "build failed m"},
				Decisive: true}},
		{"admitted stops the walk", obs("/out/x", cand("t", Built, admit), cand("m", NoGeneration, nil)),
			Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: &Target{Commit: "t", Branch: "b-t", OutPath: "/out/t"}}}},
		{"admitted main", obs("/out/x", cand("t", Built, refuse), cand("m", Built, admit)),
			Decision{Action: SubmitDeploy, Target: 1, Desired: 1, Status: Status{State: StateDeploying, Desired: &Target{Commit: "m", Branch: "b-m", OutPath: "/out/m"}}}},
		{"suspended", func() Observation { o := obs("/out/x", cand("t", NoGeneration, nil)); o.Suspended = true; return o }(),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateWaiting, Reason: "suspended"}}},
		{"no ask while a deployment is in flight", func() Observation { o := obs("/out/x", cand("t", Built, nil)); o.InFlight = true; return o }(),
			Decision{Action: Wait, Target: -1, Desired: -1, Status: Status{State: StateDeploying}}},
		{"evaluation proceeds while a deployment is in flight", func() Observation { o := obs("/out/x", cand("t", NoGeneration, nil)); o.InFlight = true; return o }(),
			Decision{Action: Eval, Target: 0, Desired: -1, Status: Status{State: StateDeploying}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDecision(t, tt.want, Decide(tt.o))
		})
	}
}

func TestDecideVanishedOutPath(t *testing.T) {
	vanished := cand("t", Built, nil)
	vanished.Realized = false
	reEvaluate := Decision{Action: Eval, Target: 0, Desired: -1, Status: Status{State: StateWaiting, Reason: "evaluating t"}}
	assertDecision(t, reEvaluate, Decide(obs("/out/x", vanished, cand("m", Built, nil))))

	withoutAsks := obs("/out/x", vanished)
	withoutAsks.AsksRun = false
	assertDecision(t, Decision{Action: Ask, Target: 0, Desired: -1}, Decide(withoutAsks))
	vanished.Verdict = admit
	withoutAsks.Candidates = []Candidate{vanished}
	assertDecision(t, reEvaluate, Decide(withoutAsks))
}

func TestDecideDeployPredicate(t *testing.T) {
	d := cand("d", Built, admit)
	targetD := &Target{Commit: "d", Branch: "b-d", OutPath: "/out/d"}
	tests := []struct {
		name   string
		mutate func(*Observation)
		want   Decision
	}{
		{"current differs", func(o *Observation) {},
			Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: targetD}}},
		{"converged", func(o *Observation) { o.Current = "/out/d" },
			Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateConverged, Desired: targetD}, Decisive: true}},
		{"converged although the latest deployment failed", func(o *Observation) {
			o.Current = "/out/d"
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10)}
		}, Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateConverged, Desired: targetD}, Decisive: true}},
		{"unknown current", func(o *Observation) { o.Current, o.CurrentErr = "", "no link" },
			Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateWaiting, Desired: targetD, Reason: "current system unknown: no link"}}},
		{"latest deployment failed", func(o *Observation) {
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10)}
		}, Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateFailed, Desired: targetD, Reason: "deploy failed d"}, Decisive: true}},
		{"a later success supersedes the failure", func(o *Observation) {
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10), deployment("/out/d", "done", 20)}
		}, Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: targetD}}},
		{"in flight", func(o *Observation) { o.InFlight = true },
			Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateDeploying}}},
		{"confirmation pending", func(o *Observation) { o.DeployConfirm.Pending = "uuid-d" },
			Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{State: StateWaiting, Desired: targetD, Reason: "awaiting deploy confirmation d"}}},
		{"confirmed", func(o *Observation) { o.DeployConfirm.Confirmed = "uuid-d" },
			Decision{Action: Deploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: targetD}, Decisive: true}},
		{"a confirmation of another generation is not this one's", func(o *Observation) { o.DeployConfirm.Confirmed = "uuid-other" },
			Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: targetD}}},
		{"a pending confirmation of another generation is replaced", func(o *Observation) { o.DeployConfirm.Pending = "uuid-other" },
			Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{State: StateDeploying, Desired: targetD}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := obs("/out/old", d)
			tt.mutate(&o)
			assertDecision(t, tt.want, Decide(o))
		})
	}
}

func TestFailedLatest(t *testing.T) {
	tests := []struct {
		name string
		rows []*protobuf.Deployment
		want bool
	}{
		{"none", nil, false},
		{"failed", []*protobuf.Deployment{deployment("/o", "failed", 5)}, true},
		{"done", []*protobuf.Deployment{deployment("/o", "done", 5)}, false},
		{"done after failed", []*protobuf.Deployment{deployment("/o", "failed", 5), deployment("/o", "done", 6)}, false},
		{"failed after done, any order", []*protobuf.Deployment{deployment("/o", "failed", 6), deployment("/o", "done", 5)}, true},
		{"failed row listed last", []*protobuf.Deployment{deployment("/o", "done", 5), deployment("/o", "failed", 6)}, true},
		{"duplicates", []*protobuf.Deployment{deployment("/o", "failed", 6), deployment("/o", "failed", 6), deployment("/o", "done", 5)}, true},
		{"running and init rows are ignored", []*protobuf.Deployment{deployment("/o", "done", 5), deployment("/o", "running", 9), deployment("/o", "init", 9)}, false},
		{"another out path", []*protobuf.Deployment{deployment("/other", "failed", 5)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, blocked := failedLatest(tt.rows, "/o", time.Unix(0, 0))
			assert.Equal(t, tt.want, blocked)
		})
	}
}
