package reconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func manualObs(o Observation) Observation {
	o.Trigger = TriggerManual
	return o
}

func confirmedObs(o Observation) Observation {
	o.DeployConfirm.Confirmed = "uuid-t"
	return o
}

// A closed sync gate takes no Eval, SubmitBuild, Ask, SubmitDeploy or Deploy
// action whatever the walk would do, and ends a manual request.
func TestDecideSyncGateClosed(t *testing.T) {
	closed := &SyncGateClosed{Reason: "update-window: opens at 23:00"}
	want := Decision{Action: Wait, Target: -1, Decisive: true,
		Status: Status{State: StateWaiting, Reason: "sync gate: update-window: opens at 23:00"}}
	walks := []struct {
		name   string
		o      Observation
		action Action
	}{
		{"never evaluated", obs("/out/x", cand("t", NoGeneration, nil)), Eval},
		{"build to confirm", obs("/out/x", cand("t", AwaitingBuild, nil)), SubmitBuild},
		{"unasked", obs("/out/x", cand("t", Built, nil)), Ask},
		{"admitted", obs("/out/x", cand("t", Built, admit)), SubmitDeploy},
		{"admitted manual", manualObs(obs("/out/x", cand("t", Built, admit))), SubmitDeploy},
		{"deploy confirmed", confirmedObs(obs("/out/x", cand("t", Built, admit))), Deploy},
	}
	for _, tt := range walks {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.action, Decide(tt.o).Action, "the walk without the gate")
			tt.o.SyncGateClosed = closed
			got := Decide(tt.o)
			assertDecision(t, want, got)
			assert.Empty(t, got.Status.Candidates)
		})
	}
	for name, o := range map[string]Observation{
		"refused":         obs("/out/x", cand("t", Built, refuse)),
		"converged":       obs("/out/t", cand("t", Built, admit)),
		"no candidate":    obs("/out/x"),
		"current unknown": obs("", cand("t", Built, admit)),
		"failed":          obs("/out/x", cand("t", EvalFailed, nil)),
	} {
		t.Run("closed over "+name, func(t *testing.T) {
			o.SyncGateClosed = closed
			assertDecision(t, want, Decide(o))
		})
	}
}

// A deployment in flight stays the status of a closed pass and the request
// waits for the pass after it.
func TestDecideSyncGateClosedDuringADeployment(t *testing.T) {
	o := obs("/out/x", cand("t", Built, admit))
	o.SyncGateClosed = &SyncGateClosed{Reason: "teleop: active"}
	o.InFlight = true
	o.InFlightTarget = &Target{Commit: "t", Branch: "b-t", OutPath: "/out/t"}
	got := Decide(o)
	assert.Equal(t, Wait, got.Action)
	assert.Equal(t, StateDeploying, got.Status.State)
	assert.False(t, got.Decisive)
}

// Suspended outranks the gate.
func TestDecideSuspendedOutranksTheSyncGate(t *testing.T) {
	o := obs("/out/x", cand("t", Built, admit))
	o.Suspended = true
	o.SyncGateClosed = &SyncGateClosed{Reason: "teleop: active"}
	got := Decide(o)
	assert.Equal(t, "suspended", got.Status.Reason)
	assert.False(t, got.Decisive)
}
