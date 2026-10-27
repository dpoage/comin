package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *rig) mainHead() string {
	for c, l := range r.labels {
		if l == "main" {
			return c
		}
	}
	return ""
}

// waitPending returns the generation waiting on the deploy confirmer.
func (r *rig) waitPending(not string) string {
	r.t.Helper()
	var uuid string
	require.Eventually(r.t, func() bool {
		uuid = r.m.DeployConfirmer.status().Submitted
		return uuid != "" && uuid != not
	}, 15*time.Second, 25*time.Millisecond, "no generation waits on the deploy confirmer")
	return uuid
}

func TestWiredPath(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	r.verdict("testing", "refuse")
	testing := r.pushTesting("file-t1")
	main := r.mainHead()
	r.start()

	r.waitDeploys(1)
	r.waitCurrent(r.out(main))
	assert.Equal(t, deployCall{out: r.out(main), operation: "switch", asks: r.exec.deployLog()[0].asks}, r.exec.deployLog()[0])
	assert.Equal(t, []string{"testing", "main"}, r.evalLabels())
	t.Logf("testing refused: deployed %v, evals %v", r.exec.deployLog(), r.evalLabels())

	r.verdict("testing", "admit")
	r.waitDeploys(2)
	r.waitCurrent(r.out(testing))
	assert.Equal(t, "test", r.exec.deployLog()[1].operation)
	assert.Equal(t, r.out(testing), r.exec.deployLog()[1].out)
	t.Logf("testing admitted: deployed %v", r.exec.deployLog()[1])

	fetchedAt := r.m.Fetcher.Latest().Remotes[0].FetchedAt.AsTime()
	r.verdict("testing", "refuse")
	r.waitDeploys(3)
	r.waitCurrent(r.out(main))
	assert.Equal(t, deployCall{out: r.out(main), operation: "switch", asks: r.exec.deployLog()[2].asks}, r.exec.deployLog()[2])
	assert.Equal(t, fetchedAt, r.m.Fetcher.Latest().Remotes[0].FetchedAt.AsTime(), "main was redeployed without a fetch")
	assert.Equal(t, []string{"testing", "main"}, r.evalLabels())
	t.Logf("testing refused again: redeployed %v with no fetch, evals %v", r.exec.deployLog()[2], r.evalLabels())

	r.exec.set(func(e *fakeExec) { e.failDeploy, e.restart, e.reboot = true, true, true })
	next := r.pushMain("file-m2")
	r.fetch()
	select {
	case code := <-r.exits:
		assert.Equal(t, 0, code)
	case <-time.After(15 * time.Second):
		t.Fatal("comin did not exit for a restart after a failed deployment with restart_comin")
	}
	dpl := r.m.storage.DeploymentList()[0]
	assert.Equal(t, "failed", dpl.Status)
	assert.Equal(t, r.out(next), dpl.Generation.OutPath)
	assert.True(t, dpl.RestartComin.GetValue())
	assert.True(t, r.m.GetState().NeedToReboot.GetValue())
	t.Logf("failing deployment with restart_comin: exit %d, deployment %s", 0, dpl.Status)
}

func TestLazyEvaluation(t *testing.T) {
	t.Run("an admitted testing candidate is the only evaluation", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		testing := r.pushTesting("file-t1")
		r.start()
		r.waitDeploys(1)
		r.settle(10)
		assert.Equal(t, []string{"testing"}, r.evalLabels())
		assert.Equal(t, []deployCall{{out: r.out(testing), operation: "test", asks: r.exec.deployLog()[0].asks}}, r.exec.deployLog())
	})

	t.Run("a refused testing candidate evaluates main", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.verdict("testing", "refuse")
		r.pushTesting("file-t1")
		r.start()
		r.waitDeploys(1)
		r.settle(10)
		assert.Equal(t, []string{"testing", "main"}, r.evalLabels())
		assert.Equal(t, r.out(r.mainHead()), r.exec.deployLog()[0].out)
	})

	t.Run("a vanished out path is evaluated once more", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		testing := r.pushTesting("file-t1")
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(testing))
		r.settle(3)
		require.NoError(t, os.RemoveAll(r.out(testing)))
		require.Eventually(t, func() bool { return len(r.evalLabels()) == 2 }, 15*time.Second, 25*time.Millisecond)
		require.Eventually(t, func() bool { return r.m.executor.IsStorePathExist(r.out(testing)) }, 15*time.Second, 25*time.Millisecond)
		r.settle(10)
		assert.Equal(t, []string{"testing", "testing"}, r.evalLabels())
		assert.Len(t, r.exec.deployLog(), 1)
	})
}

func TestLevelTriggered(t *testing.T) {
	t.Run("an external revert is redeployed without a fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.exec.setCurrent(oldSystem)
		r.waitDeploys(2)
		r.waitCurrent(r.out(r.mainHead()))
		assert.Equal(t, r.out(r.mainHead()), r.exec.deployLog()[1].out)
		assert.Equal(t, []string{"main"}, r.evalLabels())
	})

	t.Run("a failed deployment of the running out path is not retried", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.exec.set(func(e *fakeExec) { e.failDeploy, e.failMoves = true, true })
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("an unknown running system deploys nothing", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.exec.set(func(e *fakeExec) { e.unknown = true })
		r.start()
		require.Eventually(t, func() bool { return r.asksFor(r.out(r.mainHead())) > 0 }, 15*time.Second, 25*time.Millisecond)
		r.settle(10)
		assert.Empty(t, r.exec.deployLog())
		r.exec.set(func(e *fakeExec) { e.unknown = false })
		r.waitDeploys(1)
	})

	t.Run("a verdict flipped between ticks is acted on without a fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.verdict("testing", "refuse")
		testing := r.pushTesting("file-t1")
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.verdict("testing", "admit")
		r.waitDeploys(2)
		assert.Equal(t, deployCall{out: r.out(testing), operation: "test", asks: r.exec.deployLog()[1].asks}, r.exec.deployLog()[1])
		r.verdict("testing", "refuse")
		r.waitDeploys(3)
		assert.Equal(t, r.out(r.mainHead()), r.exec.deployLog()[2].out)
	})

	t.Run("a deploy confirmation is submitted once per desired change", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, autoDelay: 3 * time.Second, tick: time.Second})
		r.start()
		r.waitPending("")
		submitted := time.Now()
		time.Sleep(2 * time.Second)
		assert.Empty(t, r.exec.deployLog(), "deployed before the confirmation delay")
		r.waitDeploys(1)
		assert.GreaterOrEqual(t, time.Since(submitted), 2900*time.Millisecond)
		r.settle(3)
		assert.Len(t, r.exec.deployLog(), 1)
	})
}

func TestActivationGate(t *testing.T) {
	t.Run("every deploy is preceded by an admitting ask made after the confirmation", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, manual: true, tick: time.Hour})
		testing := r.pushTesting("file-t1")
		r.start()
		uuid := r.waitPending("")
		time.Sleep(300 * time.Millisecond)
		assert.Empty(t, r.exec.deployLog(), "a manual deploy confirmer must block the deployment")
		before := len(r.asks())
		r.m.DeployConfirmer.Confirm(uuid)
		r.waitDeploys(1)
		d := r.exec.deployLog()[0]
		assert.Equal(t, 2, d.asks-before, "one ask for the pass and one right before the activation")
		last := r.asks()[d.asks-1]
		assert.Equal(t, ask{
			branch: "testing", out: r.out(testing), sha: testing, ref: "origin/testing", msg: "file-t1",
			operation: "test", current: oldSystem, trigger: "poll", hostname: "my-machine", outExists: true,
		}, last)
		assert.Equal(t, d.out, last.out)
	})

	t.Run("an out path missing at ask time is rebuilt before any ask", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, manual: true, tick: time.Hour})
		r.start()
		uuid := r.waitPending("")
		time.Sleep(300 * time.Millisecond)
		before := len(r.asks())
		require.NoError(t, os.RemoveAll(r.out(r.mainHead())))
		r.m.wake()
		require.Eventually(t, func() bool { return len(r.evalLabels()) == 2 && r.m.DeployConfirmer.status().Submitted != uuid }, 15*time.Second, 25*time.Millisecond)
		require.Eventually(t, func() bool { return len(r.asks()) > before }, 15*time.Second, 25*time.Millisecond)
		for _, a := range r.asks() {
			assert.True(t, a.outExists, "asked about %s while it was not in the store", a.out)
		}
	})

	t.Run("a generation missing from the store is not activated", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.exec.set(func(e *fakeExec) { e.unknown = true })
		r.start()
		gone := filepath.Join(r.exec.storeDir, "gone")
		r.m.deployer.Submit(&protobuf.Generation{Uuid: "gone", OutPath: gone}, "switch")
		time.Sleep(time.Second)
		assert.Empty(t, r.exec.deployLog())
		assert.Zero(t, r.asksFor(gone))
	})

	t.Run("an activation the preflight refuses is not recorded", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.exec.set(func(e *fakeExec) { e.unknown = true })
		r.verdict("main", "refuse")
		r.start()
		require.Eventually(t, func() bool { return r.asksFor(r.out(r.mainHead())) > 0 }, 15*time.Second, 25*time.Millisecond)
		r.m.deployer.Submit(&protobuf.Generation{Uuid: "g", OutPath: r.out(r.mainHead())}, "switch")
		require.Eventually(t, func() bool { return r.m.lastActivationVerdict() != "" }, 15*time.Second, 25*time.Millisecond)
		assert.Equal(t, "refused: main", r.m.lastActivationVerdict())
		assert.Empty(t, r.exec.deployLog())
		assert.Empty(t, r.m.storage.DeploymentList())
	})

	t.Run("without a preflight command every candidate is admitted", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.pushTesting("file-t1")
		r.start()
		r.waitDeploys(1)
		assert.Equal(t, "test", r.exec.deployLog()[0].operation)
		assert.Zero(t, countLines(r.exec.askLog))
	})

	t.Run("a manual trigger reaches the ask environment", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.start()
		r.waitDeploys(1)
		r.settle(3)
		r.m.triggerCh <- "manual"
		require.Eventually(t, func() bool {
			for _, a := range r.asks() {
				if a.trigger == "manual" {
					return true
				}
			}
			return false
		}, 15*time.Second, 25*time.Millisecond)
	})
}

func (m *Manager) lastActivationVerdict() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastActivation == nil {
		return ""
	}
	return m.lastActivation.Verdict.Reason
}

func TestFailedDeployment(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	r.exec.set(func(e *fakeExec) { e.failDeploy = true })
	r.start()
	r.waitDeploys(1)
	d1 := r.exec.deployLog()[0].out
	require.Eventually(t, func() bool {
		l := r.m.storage.DeploymentList()
		return len(l) > 0 && l[0].Status == "failed"
	}, 15*time.Second, 25*time.Millisecond)
	r.settle(10)
	assert.Len(t, r.exec.deployLog(), 1, "a failed deployment of %s is retried by a poll pass", d1)

	entries, err := os.ReadDir(r.stateDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.Contains(t, []string{"store.json", "gcroots", "repository", "grpc.sock"}, e.Name())
	}

	r.stop()
	r.start()
	r.waitDeploys(2)
	r.settle(10)
	assert.Len(t, r.exec.deployLog(), 2, "a single failure from before the restart is retried once")

	r.exec.set(func(e *fakeExec) { e.failDeploy = false })
	d2 := r.pushMain("file-m2")
	r.fetch()
	r.waitDeploys(3)
	assert.Equal(t, r.out(d2), r.exec.deployLog()[2].out)
	r.waitCurrent(r.out(d2))
}

func TestResponsiveness(t *testing.T) {
	t.Run("GetState does not wait on an ask", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		r.block(true)
		r.start()
		require.Eventually(t, func() bool { return len(r.asks()) > 0 }, 15*time.Second, 25*time.Millisecond)
		for range 5 {
			start := time.Now()
			r.m.GetState()
			assert.Less(t, time.Since(start), time.Second)
			time.Sleep(100 * time.Millisecond)
		}
		assert.Empty(t, r.exec.deployLog())
	})

	t.Run("GetState does not wait on a deploy confirmation", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, manual: true})
		r.start()
		var worst atomic.Int64
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				r.m.GetState()
				if d := int64(time.Since(start)); d > worst.Load() {
					worst.Store(d)
				}
			}
		}()
		uuid := ""
		for i := range 5 {
			uuid = r.waitPending(uuid)
			time.Sleep(20 * time.Millisecond)
			r.m.DeployConfirmer.Confirm(uuid)
			r.waitDeploys(i + 1)
			r.waitCurrent(r.exec.deployLog()[i].out)
			if i < 4 {
				r.pushMain(fmt.Sprintf("file-m%d", i))
				r.fetch()
			}
		}
		assert.Less(t, time.Duration(worst.Load()), time.Second)
	})
}

func TestSuspendBlocksEveryStep(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	r.start()
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
	require.NoError(t, r.m.Suspend())
	r.settle(3)
	asks := len(r.asks())
	r.pushMain("file-m2")
	r.fetch()
	r.settle(10)
	assert.Equal(t, []string{"main"}, r.evalLabels())
	assert.Len(t, r.exec.deployLog(), 1)
	assert.Equal(t, asks, len(r.asks()))
	require.NoError(t, r.m.Resume(t.Context()))
	r.waitDeploys(2)
	assert.Len(t, r.evalLabels(), 2)
}
