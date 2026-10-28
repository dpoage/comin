package manager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSuspendInterruptsABuild(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, tick: 50 * time.Millisecond})
	r.exec.set(func(e *fakeExec) { e.buildDelay = 1500 * time.Millisecond })
	head := r.pushTesting("file-t1")
	r.start()
	require.Eventually(t, func() bool { return r.exec.buildCount() == 1 }, 15*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, r.m.Suspend())
	asks := len(r.asks())
	r.settle(10)
	assert.Equal(t, []string{"testing"}, r.evalLabels(), "evaluated while suspended")
	assert.Equal(t, 1, r.exec.buildCount(), "built while suspended")
	assert.Equal(t, asks, len(r.asks()), "asked while suspended")
	assert.Empty(t, r.exec.deployLog(), "deployed while suspended")

	require.NoError(t, r.m.Resume(t.Context()))
	r.waitDeploys(1)
	r.waitCurrent(r.out(head))
	r.settle(10)
	assert.Equal(t, []deployCall{{out: r.out(head), operation: "test", asks: r.exec.deployLog()[0].asks}}, r.exec.deployLog())
	assert.Equal(t, []string{"testing"}, r.evalLabels(), "the interrupted build is not a failure: testing is rebuilt, main is never evaluated")
}

func TestSuspendDuringThePreActivationAsk(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, manual: true, tick: time.Hour})
	r.start()
	uuid := r.waitPending("")
	before := len(r.asks())
	// The pass ask after the confirmation is fast; the pre-activation ask takes 10 s.
	r.slowAsks(10*time.Second, before+2)
	r.m.DeployConfirmer.Confirm(uuid)
	require.Eventually(t, func() bool { return len(r.asks()) == before+2 }, 15*time.Second, 10*time.Millisecond, "the pre-activation ask did not start")
	require.NoError(t, r.m.Suspend())
	time.Sleep(12 * time.Second)
	assert.Empty(t, r.exec.deployLog(), "a deployment started after Suspend returned")

	r.slowAsks(0, 0)
	require.NoError(t, r.m.Resume(t.Context()))
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
	assert.Empty(t, r.m.DeployConfirmer.status().Submitted, "the confirmation given before the suspension still holds")
}
