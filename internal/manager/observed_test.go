package manager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstAfter returns the first comin status --json document whose
// reconcile.observed_at is after t.
func firstAfter(t *testing.T, r *rig, after time.Time) map[string]any {
	t.Helper()
	var v map[string]any
	require.Eventually(t, func() bool {
		v = statusStep(t, r)
		s, ok := reconcileField(t, v, "observed_at").(string)
		if !ok {
			return false
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return at.After(after)
	}, 15*time.Second, 5*time.Millisecond, "no status observed after %s", after)
	return v
}

// holdAPass makes the next ask hold its pass past its verdict read and
// returns once it read that verdict: the pass started before the return.
func holdAPass(t *testing.T, r *rig) {
	t.Helper()
	r.lateAsks(time.Second)
	held := r.lateReads()
	require.Eventually(t, func() bool { return r.lateReads() > held }, 15*time.Second, 5*time.Millisecond, "no ask read its verdict")
}

// The first status observed after an action reflects a pass that started
// after it, even when a pass that read its inputs before the action ends
// after it.
func TestObservedAtFollowsTheAction(t *testing.T) {
	t.Run("a verdict flip", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		testing := r.pushTesting("file-t1")
		r.start()
		v := waitState(t, r, "converged")
		assertDesired(t, v, map[string]any{"commit": testing, "branch": "testing", "out_path": r.out(testing)})

		holdAPass(t, r)
		at := time.Now()
		r.verdict("testing", "refuse")
		r.lateAsks(0)

		v = firstAfter(t, r, at)
		assert.Equal(t, "refused", outcomes(t, v)[0], "the testing head refused after the flip")
		assert.NotEqual(t, "converged", reconcileField(t, v, "state"))
		r.waitCurrent(r.out(r.mainHead()))
	})

	t.Run("switch-latest", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		testing := r.pushTesting("file-t1")
		r.verdict("testing", "manual")
		r.verdict("main", "refuse")
		r.start()
		require.Eventually(t, func() bool {
			cs := r.m.GetState().GetReconcile().GetCandidates()
			return len(cs) == 2 && cs[0].GetOutcome() == "refused" && cs[1].GetOutcome() == "refused"
		}, 15*time.Second, 25*time.Millisecond, "the poll passes never refused both candidates")

		holdAPass(t, r)
		at := time.Now()
		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.lateAsks(0)

		v := firstAfter(t, r, at)
		assert.Equal(t, []string{"admitted", "skipped"}, outcomes(t, v), "the manual pass admits the testing head")
		assertDesired(t, v, map[string]any{"commit": testing, "branch": "testing", "out_path": r.out(testing)})
		r.waitCurrent(r.out(testing))
	})
}
