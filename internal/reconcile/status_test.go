package reconcile

import (
	"testing"

	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
)

func target(name string) *Target {
	return &Target{Commit: name, Branch: "b-" + name, OutPath: "/out/" + name}
}

func failedRow(out, errMsg string, endedAt int64) *protobuf.Deployment {
	d := deployment(out, "failed", endedAt)
	d.ErrorMsg = errMsg
	return d
}

func TestStatusRows(t *testing.T) {
	d := cand("d", Built, admit)
	admittedD := []CandidateStatus{walkEntry("d", OutcomeAdmitted, "")}
	tests := []struct {
		name string
		o    Observation
		want Status
	}{
		{"a deployment in flight", func() Observation {
			o := obs("/out/old", cand("t", Built, admit))
			o.InFlight, o.InFlightTarget = true, target("x")
			return o
		}(), Status{State: StateDeploying, Desired: target("x"), Candidates: []CandidateStatus{walkEntry("t", OutcomeAdmitted, "")}}},
		{"a candidate not asked while a deployment is in flight", func() Observation {
			o := obs("/out/old", cand("t", Built, nil), cand("m", Built, nil))
			o.InFlight, o.InFlightTarget = true, target("x")
			return o
		}(), Status{State: StateDeploying, Desired: target("x"), Candidates: []CandidateStatus{
			walkEntry("t", OutcomePending, "deployment in flight x"), walkEntry("m", OutcomeSkipped, "")}}},
		{"suspended", func() Observation {
			o := obs("/out/x", cand("t", Built, admit))
			o.Suspended = true
			return o
		}(), Status{State: StateWaiting, Reason: "suspended"}},
		{"evaluating", obs("/out/x", cand("t", Evaluating, nil)),
			Status{State: StateWaiting, Reason: "evaluating t", Candidates: []CandidateStatus{walkEntry("t", OutcomePending, "evaluating t")}}},
		{"building", obs("/out/x", cand("t", Building, nil)),
			Status{State: StateWaiting, Reason: "building t", Candidates: []CandidateStatus{walkEntry("t", OutcomePending, "building t")}}},
		{"awaiting build confirmation", func() Observation {
			c := cand("t", AwaitingBuild, nil)
			c.BuildSubmitted = true
			return obs("/out/x", c)
		}(), Status{State: StateWaiting, Reason: "awaiting build confirmation t", Candidates: []CandidateStatus{walkEntry("t", OutcomePending, "awaiting build confirmation t")}}},
		{"never evaluated", obs("/out/x", cand("t", NoGeneration, nil)),
			Status{State: StateWaiting, Reason: "evaluating t", Candidates: []CandidateStatus{walkEntry("t", OutcomePending, "evaluating t")}}},
		{"current unknown", func() Observation {
			o := obs("", d)
			o.CurrentErr = "no link"
			return o
		}(), Status{State: StateWaiting, Desired: target("d"), Reason: "current system unknown: no link", Candidates: admittedD}},
		{"current unknown without an error text", obs("", d),
			Status{State: StateWaiting, Desired: target("d"), Reason: "current system unknown", Candidates: admittedD}},
		{"converged", obs("/out/d", d),
			Status{State: StateConverged, Desired: target("d"), Candidates: admittedD}},
		{"converged with a failed row for the out path", func() Observation {
			o := obs("/out/d", d)
			o.Deployments = []*protobuf.Deployment{failedRow("/out/d", "switch failed", 10)}
			return o
		}(), Status{State: StateConverged, Desired: target("d"), Candidates: admittedD}},
		{"the last deployment of the out path failed", func() Observation {
			o := obs("/out/old", d)
			o.Deployments = []*protobuf.Deployment{failedRow("/out/d", "switch failed", 10)}
			return o
		}(), Status{State: StateFailed, Desired: target("d"), Reason: "deploy failed d: switch failed", Candidates: admittedD}},
		{"awaiting deploy confirmation", func() Observation {
			o := obs("/out/old", d)
			o.DeployConfirm.Pending = "uuid-d"
			return o
		}(), Status{State: StateWaiting, Desired: target("d"), Reason: "awaiting deploy confirmation d", Candidates: admittedD}},
		{"otherwise deploying", obs("/out/old", d),
			Status{State: StateDeploying, Desired: target("d"), Candidates: admittedD}},
		{"walk exhausted on an eval failure", func() Observation {
			c := cand("t", EvalFailed, nil)
			c.Err = "attr not found"
			return obs("/out/x", c)
		}(), Status{State: StateFailed, Desired: target("t"), Reason: "eval failed t: attr not found", Candidates: []CandidateStatus{walkEntry("t", OutcomeFailed, "eval failed t: attr not found")}}},
		{"walk exhausted on a build failure", obs("/out/x", cand("t", BuildFailed, nil)),
			Status{State: StateFailed, Desired: target("t"), Reason: "build failed t", Candidates: []CandidateStatus{walkEntry("t", OutcomeFailed, "build failed t")}}},
		{"walk exhausted on a machine-id mismatch", obs("/out/x", cand("t", MachineIdMismatch, nil)),
			Status{State: StateFailed, Desired: target("t"), Reason: "machine-id mismatch t", Candidates: []CandidateStatus{walkEntry("t", OutcomeFailed, "machine-id mismatch t")}}},
		{"walk exhausted on an unsigned commit", func() Observation {
			c := cand("t", Built, nil)
			c.Verified = false
			return obs("/out/x", c)
		}(), Status{State: StateFailed, Desired: target("t"), Reason: "unsigned commit t", Candidates: []CandidateStatus{walkEntry("t", OutcomeFailed, "unsigned commit t")}}},
		{"a failed candidate below a pending stop is not the failure", obs("/out/x", cand("t", BuildFailed, nil), cand("m", Evaluating, nil)),
			Status{State: StateWaiting, Reason: "evaluating m", Candidates: []CandidateStatus{
				walkEntry("t", OutcomeFailed, "build failed t"), walkEntry("m", OutcomePending, "evaluating m")}}},
		{"a failed candidate below an admitted one is not the failure", obs("/out/m", cand("t", BuildFailed, nil), cand("m", Built, admit)),
			Status{State: StateConverged, Desired: target("m"), Candidates: []CandidateStatus{
				walkEntry("t", OutcomeFailed, "build failed t"), walkEntry("m", OutcomeAdmitted, "")}}},
		{"all refused keeps the first refused line", func() Observation {
			second := cand("m", Built, &preflight.Verdict{Reason: "refused: gate-b"})
			return obs("/out/x", cand("t", Built, refuse), second)
		}(), Status{State: StateWaiting, Reason: "refused: gate", Candidates: []CandidateStatus{
			walkEntry("t", OutcomeRefused, "refused: gate"), walkEntry("m", OutcomeRefused, "refused: gate-b")}}},
		{"a refusal below an admitted candidate", func() Observation {
			return obs("/out/m", cand("t", Built, refuse), cand("m", Built, admit))
		}(), Status{State: StateConverged, Desired: target("m"), Candidates: []CandidateStatus{
			walkEntry("t", OutcomeRefused, "refused: gate"), walkEntry("m", OutcomeAdmitted, "")}}},
		{"candidates below a pending stop are skipped", func() Observation {
			return obs("/out/x", cand("t", Evaluating, nil), cand("m", Built, admit))
		}(), Status{State: StateWaiting, Reason: "evaluating t", Candidates: []CandidateStatus{
			walkEntry("t", OutcomePending, "evaluating t"), walkEntry("m", OutcomeSkipped, "")}}},
		{"a failure takes precedence over a refusal", obs("/out/x", cand("t", Built, refuse), cand("m", BuildFailed, nil)),
			Status{State: StateFailed, Desired: target("m"), Reason: "build failed m", Candidates: []CandidateStatus{
				walkEntry("t", OutcomeRefused, "refused: gate"), walkEntry("m", OutcomeFailed, "build failed m")}}},
		{"no candidate", obs("/out/x"),
			Status{State: StateWaiting, Reason: "no candidate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Decide(tt.o).Status)
		})
	}
}

// The top-level reason is the reason of the entry that decided the walk.
func TestStatusReasonIsTheDecidingEntryReason(t *testing.T) {
	for _, o := range []Observation{
		obs("/out/x", cand("t", Evaluating, nil), cand("m", Built, admit)),
		obs("/out/x", cand("t", Built, refuse), cand("m", Built, &preflight.Verdict{Reason: "refused: gate-b"})),
		obs("/out/x", cand("t", Built, refuse), cand("m", BuildFailed, nil)),
	} {
		st := Decide(o).Status
		assert.NotEmpty(t, st.Candidates)
		assert.Equal(t, decidingReason(st), st.Reason, "candidates %v", st.Candidates)
	}
}

// decidingReason returns the reason of the entry that stopped the walk:
// the pending one, the first refusal when all refused, the first failure
// when one failed.
func decidingReason(st Status) string {
	var firstRefused, firstFailed string
	for _, c := range st.Candidates {
		switch c.Outcome {
		case OutcomePending:
			return c.Reason
		case OutcomeRefused:
			if firstRefused == "" {
				firstRefused = c.Reason
			}
		case OutcomeFailed:
			if firstFailed == "" {
				firstFailed = c.Reason
			}
		}
	}
	if firstFailed != "" {
		return firstFailed
	}
	return firstRefused
}

func TestStatusManualRetriesABlockedOutPath(t *testing.T) {
	o := obs("/out/old", cand("d", Built, admit))
	o.Deployments = []*protobuf.Deployment{failedRow("/out/d", "switch failed", 10)}
	o.Trigger = TriggerManual
	assert.Equal(t, Decision{Action: SubmitDeploy, Target: 0, Desired: 0, Status: Status{
		State: StateDeploying, Desired: target("d"),
		Candidates: []CandidateStatus{walkEntry("d", OutcomeAdmitted, "")}}}, Decide(o))

	o.Trigger = TriggerPoll
	assert.Equal(t, Decision{Action: Wait, Target: -1, Desired: 0, Status: Status{
		State: StateFailed, Desired: target("d"), Reason: "deploy failed d: switch failed",
		Candidates: []CandidateStatus{walkEntry("d", OutcomeAdmitted, "")}}, Decisive: true}, Decide(o))
}

// A walk that concludes ends a manual request, all refused included, unless
// a deployment is in flight: the request then waits for the pass after it.
func TestDecisiveWalkEnds(t *testing.T) {
	for _, tt := range []struct {
		name string
		o    Observation
	}{
		{"all refused", obs("/out/x", cand("t", Built, refuse), cand("m", Built, refuse))},
		{"a candidate failed", obs("/out/x", cand("t", Built, refuse), cand("m", BuildFailed, nil))},
		{"no candidate", obs("/out/x")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.o)
			assert.Equal(t, Wait, d.Action)
			assert.True(t, d.Decisive, "the walk concluded: %+v", d.Status)

			tt.o.InFlight, tt.o.InFlightTarget = true, target("x")
			d = Decide(tt.o)
			assert.Equal(t, Wait, d.Action)
			assert.False(t, d.Decisive, "a deployment is in flight: %+v", d.Status)
		})
	}
}
