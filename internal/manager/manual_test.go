package manager

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/reconcile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManualTriggerAsks(t *testing.T) {
	t.Run("a verdict that admits only manual deploys only after switch-latest", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		testing := r.pushTesting("file-t1")
		r.verdict("testing", "refuse")
		r.verdict("main", "manual")
		r.start()

		r.settle(10)
		assert.Empty(t, r.exec.deployLog(), "a poll pass is refused by a manual-only verdict")
		before := len(r.asks())
		for _, a := range r.asks()[:before] {
			assert.Equal(t, reconcile.TriggerPoll, a.trigger)
		}

		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))

		asked := map[string]bool{}
		for _, a := range r.asks()[before:] {
			asked[a.branch+"@"+a.trigger] = true
		}
		assert.True(t, asked["testing@manual"], "the asks of the manual pass: %v", r.asks())
		assert.True(t, asked["main@manual"], "the asks of the manual pass: %v", r.asks())
		assert.Equal(t, r.out(r.mainHead()), r.exec.deployLog()[0].out)
		assert.NotEqual(t, r.out(testing), r.exec.deployLog()[0].out)
	})

	t.Run("a request held behind a deploy confirmation reaches the activation ask", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, manual: true})
		testing := r.pushTesting("file-t1")
		r.start()
		uuid := r.waitPending("")

		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.verdict("testing", "manual")
		r.m.DeployConfirmer.Confirm(uuid)

		r.waitDeploys(1)
		r.waitCurrent(r.out(testing))
		d := r.exec.deployLog()[0]
		activation := r.asks()[d.asks-1]
		assert.Equal(t, reconcile.TriggerManual, activation.trigger, "the pre-activation ask must carry the manual trigger: %v", r.asks())
		assert.Equal(t, r.out(testing), activation.out)
	})

	t.Run("a request during a deployment is settled by the pass that follows it", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.exec.set(func(e *fakeExec) { e.deployDelay = 2 * time.Second })
		r.start()

		r.waitDeploys(1)
		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.waitCurrent(r.out(r.mainHead()))

		// The deployment the request met converged the system: the next
		// tick asks go back to poll.
		var last ask
		require.Eventually(t, func() bool {
			asks := r.asks()
			if len(asks) == 0 {
				return false
			}
			last = asks[len(asks)-1]
			return last.trigger == reconcile.TriggerPoll
		}, 15*time.Second, 25*time.Millisecond, "the request outlived the deployment that served it: %v", r.asks())
	})
}

func TestSwitchDeploymentLatestRetriesOnce(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	testing := r.pushTesting("file-t1")
	r.verdict("testing", "manual")
	r.verdict("main", "refuse")
	r.exec.set(func(e *fakeExec) { e.failDeploy = true })
	r.start()

	r.settle(10)
	assert.Empty(t, r.exec.deployLog(), "a poll pass is refused by a manual-only verdict")

	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitDeploys(1)
	assert.Equal(t, r.out(testing), r.exec.deployLog()[0].out)

	r.settle(10)
	assert.Len(t, r.exec.deployLog(), 1, "poll passes do not retry the failed deployment")
}

// waitManualPass waits for the manual ask of main logged from the from-th
// ask on, the last ask of a manual pass over both candidates, and returns
// its index.
func waitManualPass(t *testing.T, r *rig, from int) int {
	t.Helper()
	at := -1
	require.Eventually(t, func() bool {
		for i, a := range r.asks() {
			if i >= from && a.branch == "main" && a.trigger == reconcile.TriggerManual {
				at = i
				return true
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond, "no manual pass: %v", r.asks())
	return at
}

func TestSwitchDeploymentLatestEndsAtAnAllRefusedPass(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	r.pushTesting("file-t1")
	r.verdict("testing", "refuse")
	r.verdict("main", "refuse")
	r.start()
	r.settle(10)

	before := len(r.asks())
	require.NoError(t, r.m.SwitchDeploymentLatest())
	served := waitManualPass(t, r, before)

	// Every candidate refused the manual pass: the request is over and
	// the passes that follow ask with poll.
	r.settle(10)
	later := r.asks()[served+1:]
	require.NotEmpty(t, later, "no pass after the manual one")
	for _, a := range later {
		assert.Equal(t, reconcile.TriggerPoll, a.trigger, "an ask after the all-refused manual pass: %v", later)
	}

	// A verdict that admits only manual opened afterwards deploys nothing
	// without a new request.
	r.verdict("main", "manual")
	r.settle(10)
	assert.Empty(t, r.exec.deployLog(), "the ended request deployed")
}

func TestSwitchDeploymentLatestRetryIsSingleUse(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	testing := r.pushTesting("file-t1")
	r.verdict("testing", "manual")
	r.verdict("main", "refuse")
	r.exec.set(func(e *fakeExec) { e.failDeploy = true })
	r.start()
	r.settle(10)

	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitDeploys(1)
	assert.Equal(t, r.out(testing), r.exec.deployLog()[0].out)
	r.settle(5)

	// A second request meets a pass where every candidate refuses: it
	// retries nothing, and it ends there.
	r.verdict("testing", "refuse")
	before := len(r.asks())
	require.NoError(t, r.m.SwitchDeploymentLatest())
	waitManualPass(t, r, before)

	// The testing gate opens for poll passes: the failed deployment is
	// not retried without a new request.
	r.verdict("testing", "admit")
	r.settle(10)
	assert.Len(t, r.exec.deployLog(), 1, "the retry was used by a pass that served no request")
}
