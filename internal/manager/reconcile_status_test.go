package manager

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// statusJSON options are the ones of comin status --json.
var statusJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// statusStep returns the parsed comin status --json document and logs it.
func statusStep(t *testing.T, r *rig) map[string]any {
	t.Helper()
	buf, err := statusJSON.Marshal(r.m.GetState())
	require.NoError(t, err)
	t.Logf("status: %s", buf)
	var v map[string]any
	require.NoError(t, json.Unmarshal(buf, &v))
	return v
}

func reconcileKeys(t *testing.T, v map[string]any) []string {
	t.Helper()
	rc, ok := v["reconcile"].(map[string]any)
	require.True(t, ok, "no reconcile object in %v", v)
	keys := make([]string, 0, len(rc))
	for k := range rc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func reconcileField(t *testing.T, v map[string]any, field string) any {
	t.Helper()
	return v["reconcile"].(map[string]any)[field]
}

// outcomes reads the per-candidate outcome sequence of a status document.
func outcomes(t *testing.T, v map[string]any) []string {
	t.Helper()
	cs, ok := reconcileField(t, v, "candidates").([]any)
	require.True(t, ok, "no candidates list in %v", v)
	var out []string
	for _, c := range cs {
		out = append(out, c.(map[string]any)["outcome"].(string))
	}
	return out
}

// candidateKeys checks the keys of the first candidate entry.
func candidateKeys(t *testing.T, v map[string]any) {
	t.Helper()
	cs := reconcileField(t, v, "candidates").([]any)
	require.NotEmpty(t, cs)
	keys := make([]string, 0)
	for k := range cs[0].(map[string]any) {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	assert.Equal(t, []string{"branch", "commit", "kind", "out_path", "outcome", "reason"}, keys)
}

func waitState(t *testing.T, r *rig, state string) map[string]any {
	t.Helper()
	require.Eventually(t, func() bool {
		return r.m.GetState().GetReconcile().GetState() == state
	}, 15*time.Second, 25*time.Millisecond, "reconcile state never reached %s", state)
	return statusStep(t, r)
}

func assertDesired(t *testing.T, v map[string]any, want map[string]any) {
	t.Helper()
	assert.Equal(t, want, reconcileField(t, v, "desired"))
}

func TestStatusArtifact(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	r.verdict("testing", "refuse")
	testing := r.pushTesting("file-t1")
	main := r.mainHead()
	r.start()

	// The testing head is refused: main is deployed and converged.
	v := waitState(t, r, "converged")
	assert.Equal(t, []string{"candidates", "current", "desired", "reason", "since", "state"}, reconcileKeys(t, v))
	_, drift := v["drift"]
	assert.False(t, drift, "drift must be gone")
	assert.Equal(t, "", reconcileField(t, v, "reason"))
	assertDesired(t, v, map[string]any{"commit": main, "branch": "main", "out_path": r.out(main)})
	assert.Equal(t, r.out(main), reconcileField(t, v, "current"))
	candidateKeys(t, v)
	assert.Equal(t, []string{"refused", "admitted"}, outcomes(t, v), "the testing head refused, main deployed")
	rc := reconcileField(t, v, "candidates").([]any)[0].(map[string]any)
	assert.Equal(t, "testing", rc["kind"])
	assert.Equal(t, "refused: testing", rc["reason"])
	sinceConverged := reconcileField(t, v, "since")

	// Converged stays converged across ticks: since does not move.
	r.settle(10)
	v = statusStep(t, r)
	assert.Equal(t, "converged", reconcileField(t, v, "state"))
	assert.Equal(t, sinceConverged, reconcileField(t, v, "since"), "since moved while the state stayed converged")

	// Every candidate is refused after that deployment: desired is null.
	r.verdict("main", "refuse")
	v = waitState(t, r, "waiting")
	assert.Equal(t, "refused: testing", reconcileField(t, v, "reason"))
	assert.Equal(t, nil, reconcileField(t, v, "desired"))
	assert.Equal(t, []string{"refused", "refused"}, outcomes(t, v), "all refused after a prior deployment")
	sinceWaiting := reconcileField(t, v, "since")
	assert.NotEqual(t, sinceConverged, sinceWaiting, "since must change with the state")
	r.settle(10)
	v = statusStep(t, r)
	assert.Equal(t, "waiting", reconcileField(t, v, "state"))
	assert.Equal(t, sinceWaiting, reconcileField(t, v, "since"), "since moved while the state stayed waiting")

	// The testing head is admitted: the state passes through deploying
	// with the in-flight target, then converges on testing.
	r.exec.set(func(e *fakeExec) { e.deployDelay = time.Second })
	r.verdict("testing", "admit")
	v = waitState(t, r, "deploying")
	assertDesired(t, v, map[string]any{"commit": testing, "branch": "testing", "out_path": r.out(testing)})
	assert.Equal(t, []string{"admitted", "skipped"}, outcomes(t, v), "the testing head admitted")
	v = waitState(t, r, "converged")
	assertDesired(t, v, map[string]any{"commit": testing, "branch": "testing", "out_path": r.out(testing)})
	assert.NotEqual(t, sinceWaiting, reconcileField(t, v, "since"), "since must change with the state")

	// A manual-only verdict leaves the poll passes refused; the request
	// settles on the converged candidate it asked for.
	r.verdict("testing", "manual")
	v = waitState(t, r, "waiting")
	assert.Equal(t, "refused: testing", reconcileField(t, v, "reason"))
	assert.Equal(t, nil, reconcileField(t, v, "desired"))
	assert.Equal(t, []string{"refused", "refused"}, outcomes(t, v), "a manual-only verdict refuses the poll passes")
	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.verdict("testing", "admit")
	v = waitState(t, r, "converged")
	assertDesired(t, v, map[string]any{"commit": testing, "branch": "testing", "out_path": r.out(testing)})
}
