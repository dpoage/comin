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
		Candidate:      repository.Candidate{CommitId: name, Verified: true},
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

func deployment(out, status string, endedAt int64) *protobuf.Deployment {
	return &protobuf.Deployment{
		Generation: &protobuf.Generation{OutPath: out},
		Status:     status,
		EndedAt:    timestamppb.New(time.Unix(endedAt, 0)),
	}
}

func TestDecideWalk(t *testing.T) {
	unsigned := cand("t", Built, nil)
	unsigned.Verified = false
	tests := []struct {
		name string
		o    Observation
		want Decision
	}{
		{"no candidate", obs("/out/x"), Decision{Wait, -1, -1}},
		{"never evaluated", obs("/out/x", cand("t", NoGeneration, nil), cand("m", NoGeneration, nil)), Decision{Eval, 0, -1}},
		{"evaluating stops the walk", obs("/out/x", cand("t", Evaluating, nil), cand("m", NoGeneration, nil)), Decision{Wait, -1, -1}},
		{"building stops the walk", obs("/out/x", cand("t", Building, nil), cand("m", NoGeneration, nil)), Decision{Wait, -1, -1}},
		{"build to confirm", obs("/out/x", cand("t", AwaitingBuild, nil), cand("m", NoGeneration, nil)), Decision{SubmitBuild, 0, -1}},
		{"build confirmation pending stops the walk", obs("/out/x", func() Candidate { c := cand("t", AwaitingBuild, nil); c.BuildSubmitted = true; return c }(), cand("m", NoGeneration, nil)), Decision{Wait, -1, -1}},
		{"unasked is asked before anything below", obs("/out/x", cand("t", Built, nil), cand("m", NoGeneration, nil)), Decision{Ask, 0, -1}},
		{"refused passes to the next", obs("/out/x", cand("t", Built, refuse), cand("m", NoGeneration, nil)), Decision{Eval, 1, -1}},
		{"refused then unasked", obs("/out/x", cand("t", Built, refuse), cand("m", Built, nil)), Decision{Ask, 1, -1}},
		{"eval failed passes", obs("/out/x", cand("t", EvalFailed, nil), cand("m", NoGeneration, nil)), Decision{Eval, 1, -1}},
		{"build failed passes", obs("/out/x", cand("t", BuildFailed, nil), cand("m", Built, nil)), Decision{Ask, 1, -1}},
		{"machine-id mismatch passes", obs("/out/x", cand("t", MachineIdMismatch, nil), cand("m", Built, nil)), Decision{Ask, 1, -1}},
		{"unsigned passes without evaluating", obs("/out/x", unsigned, cand("m", Built, nil)), Decision{Ask, 1, -1}},
		{"all failed or refused", obs("/out/x", cand("t", Built, refuse), cand("m", BuildFailed, nil)), Decision{Wait, -1, -1}},
		{"admitted stops the walk", obs("/out/x", cand("t", Built, admit), cand("m", NoGeneration, nil)), Decision{SubmitDeploy, 0, 0}},
		{"admitted main", obs("/out/x", cand("t", Built, refuse), cand("m", Built, admit)), Decision{SubmitDeploy, 1, 1}},
		{"suspended", func() Observation { o := obs("/out/x", cand("t", NoGeneration, nil)); o.Suspended = true; return o }(), Decision{Wait, -1, -1}},
		{"no ask while a deployment is in flight", func() Observation { o := obs("/out/x", cand("t", Built, nil)); o.InFlight = true; return o }(), Decision{Wait, -1, -1}},
		{"evaluation proceeds while a deployment is in flight", func() Observation { o := obs("/out/x", cand("t", NoGeneration, nil)); o.InFlight = true; return o }(), Decision{Eval, 0, -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Decide(tt.o))
		})
	}
}

func TestDecideVanishedOutPath(t *testing.T) {
	vanished := cand("t", Built, nil)
	vanished.Realized = false
	assert.Equal(t, Decision{Eval, 0, -1}, Decide(obs("/out/x", vanished, cand("m", Built, nil))), "evaluated again before any ask")

	withoutAsks := obs("/out/x", vanished)
	withoutAsks.AsksRun = false
	assert.Equal(t, Decision{Ask, 0, -1}, Decide(withoutAsks))
	vanished.Verdict = admit
	withoutAsks.Candidates = []Candidate{vanished}
	assert.Equal(t, Decision{Eval, 0, -1}, Decide(withoutAsks), "a deployment of an unrealized out path is evaluated again first")
}

func TestDecideDeployPredicate(t *testing.T) {
	d := cand("d", Built, admit)
	tests := []struct {
		name   string
		mutate func(*Observation)
		want   Decision
	}{
		{"current differs", func(o *Observation) {}, Decision{SubmitDeploy, 0, 0}},
		{"converged", func(o *Observation) { o.Current = "/out/d" }, Decision{Wait, -1, 0}},
		{"converged although the latest deployment failed", func(o *Observation) {
			o.Current = "/out/d"
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10)}
		}, Decision{Wait, -1, 0}},
		{"unknown current", func(o *Observation) { o.Current, o.CurrentErr = "", "no link" }, Decision{Wait, -1, 0}},
		{"latest deployment failed", func(o *Observation) {
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10)}
		}, Decision{Wait, -1, 0}},
		{"a later success supersedes the failure", func(o *Observation) {
			o.Deployments = []*protobuf.Deployment{deployment("/out/d", "failed", 10), deployment("/out/d", "done", 20)}
		}, Decision{SubmitDeploy, 0, 0}},
		{"in flight", func(o *Observation) { o.InFlight = true }, Decision{Wait, -1, 0}},
		{"confirmation pending", func(o *Observation) { o.DeployConfirm.Pending = "uuid-d" }, Decision{Wait, -1, 0}},
		{"confirmed", func(o *Observation) { o.DeployConfirm.Confirmed = "uuid-d" }, Decision{Deploy, 0, 0}},
		{"a confirmation of another generation is not this one's", func(o *Observation) { o.DeployConfirm.Confirmed = "uuid-other" }, Decision{SubmitDeploy, 0, 0}},
		{"a pending confirmation of another generation is replaced", func(o *Observation) { o.DeployConfirm.Pending = "uuid-other" }, Decision{SubmitDeploy, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := obs("/out/old", d)
			tt.mutate(&o)
			assert.Equal(t, tt.want, Decide(o))
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
			assert.Equal(t, tt.want, failedLatest(tt.rows, "/o", time.Unix(0, 0)))
		})
	}
}
