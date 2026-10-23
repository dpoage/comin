package manager

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// stateJSON marshals the State as cmd/status.go's jsonStatus does.
func stateJSON(t *testing.T, r *rig) map[string]any {
	t.Helper()
	buf, err := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
		Indent:          "  ",
		AllowPartial:    true,
	}.Marshal(r.m.GetState())
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(buf, &out))
	return out
}

func entryAt(t *testing.T, state map[string]any, field string) map[string]any {
	t.Helper()
	preDeploy, ok := state["pre_deploy"].(map[string]any)
	require.True(t, ok, "pre_deploy is not an object in %v", state["pre_deploy"])
	entry, _ := preDeploy[field].(map[string]any)
	return entry
}

func TestStatePreDeployRunningAndPending(t *testing.T) {
	r, h := newHookRig(t)
	h.set(t, "block-all")

	before := time.Now().Add(-time.Second)
	r.m.submitAdmitted(mainGeneration("m1", "/nix/store/m1"), "switch")
	h.waitStarts(t, "gen-m1", 1)
	r.m.submitAdmitted(mainGeneration("m2", "/nix/store/m2"), "switch")
	waitFor(t, func() bool { return r.m.GetState().GetPreDeploy().GetPending() != nil }, "m2 never became pending")
	after := time.Now().Add(time.Second)

	state := stateJSON(t, r)
	running := entryAt(t, state, "running")
	require.NotNil(t, running, "pre_deploy.running is null: %v", state["pre_deploy"])
	assert.Equal(t, "gen-m1", running["generation"].(map[string]any)["uuid"])
	assert.Equal(t, "switch", running["operation"])
	startedAt, ok := running["started_at"].(string)
	require.True(t, ok, "pre_deploy.running.started_at is %v", running["started_at"])
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	require.NoError(t, err)
	assert.False(t, started.Before(before), "started_at %s precedes the run window", started)
	assert.False(t, started.After(after), "started_at %s follows the run window", started)

	pending := entryAt(t, state, "pending")
	require.NotNil(t, pending, "pre_deploy.pending is null: %v", state["pre_deploy"])
	assert.Equal(t, "gen-m2", pending["generation"].(map[string]any)["uuid"])
	assert.Nil(t, pending["started_at"], "a pending entry has not started")

	h.clear("block-all")
	r.waitDeploys(t, "m2/switch")
	waitFor(t, r.stageIdle, "the stage never emptied")
	idle := stateJSON(t, r)
	assert.Nil(t, entryAt(t, idle, "running"))
	assert.Nil(t, entryAt(t, idle, "pending"))
}

func TestStatePreDeployNullHookIsPresentAndEmpty(t *testing.T) {
	r := newRig(t, t.TempDir())
	require.Nil(t, r.m.preDeploy)
	r.start(t)
	state := stateJSON(t, r)
	preDeploy, ok := state["pre_deploy"].(map[string]any)
	require.True(t, ok, "pre_deploy is %v, want an object", state["pre_deploy"])
	assert.Equal(t, map[string]any{"running": nil, "pending": nil}, preDeploy)
}
