package manager

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *rig) waitEvals(n int) {
	r.t.Helper()
	require.Eventually(r.t, func() bool { return len(r.exec.evalLog()) >= n }, 15*time.Second, 25*time.Millisecond, "evals: %v", r.evalLabels())
}

// failFirst fails the first n evaluations of every commit.
func failFirst(n int) func(string, int) error {
	return func(_ string, attempt int) error {
		if attempt <= n {
			return errors.New("torn HEAD")
		}
		return nil
	}
}

func reason(t *testing.T, r *rig) string {
	t.Helper()
	return r.m.GetState().GetReconcile().GetReason()
}

func TestFailedEvalIsReattemptedOncePerFetch(t *testing.T) {
	t.Run("a transient failure deploys after one fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) { e.evalErr = failFirst(1) })
		r.start()
		r.waitEvals(1)
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "eval failed "+r.mainHead()+": ")
		r.settle(10)
		assert.Len(t, r.exec.evalLog(), 1, "a tick evaluated again")
		assert.Empty(t, r.exec.deployLog())

		r.fetch()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, []string{"main", "main"}, r.evalLabels())
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("an eval that always fails is evaluated once per fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) { e.evalErr = failFirst(1 << 30) })
		r.start()
		r.waitEvals(1)
		r.settle(10)
		require.Len(t, r.exec.evalLog(), 1, "10 ticks without a fetch evaluated again")
		const fetches = 3
		for i := 1; i <= fetches; i++ {
			r.fetch()
			r.waitEvals(i + 1)
			r.settle(10)
			require.Len(t, r.exec.evalLog(), i+1, "after fetch %d", i)
		}
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "eval failed "+r.mainHead()+": ")
		assert.Empty(t, r.exec.deployLog())
	})

	t.Run("a fetch never evaluates again a generation that did not fail", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.start()
		r.waitDeploys(1)
		for range 3 {
			r.fetch()
			r.settle(5)
		}
		assert.Equal(t, []string{"main"}, r.evalLabels())
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("switch-latest evaluates a failed candidate once per request", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) { e.evalErr = failFirst(3) })
		r.start()
		r.waitEvals(1)
		waitState(t, r, "failed")
		for i := 1; i <= 2; i++ {
			require.NoError(t, r.m.SwitchDeploymentLatest())
			r.waitEvals(i + 1)
			r.settle(10)
			require.Len(t, r.exec.evalLog(), i+1, "after request %d", i)
		}
		assert.Empty(t, r.exec.deployLog())

		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.waitDeploys(1)
		r.settle(10)
		assert.Len(t, r.exec.evalLog(), 4)
	})
}

func TestFailedBuildIsReattemptedOncePerFetch(t *testing.T) {
	t.Run("a transient failure deploys after one fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) {
			e.buildErr = func(_ string, attempt int) error {
				if attempt == 1 {
					return errors.New("substituter down")
				}
				return nil
			}
		})
		r.start()
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "build failed "+r.mainHead()+": ")
		r.settle(10)
		assert.Equal(t, 1, r.exec.buildCount(), "a tick built again")
		assert.Empty(t, r.exec.deployLog())

		r.fetch()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 2, r.exec.buildCount())
		assert.Equal(t, []string{"main", "main"}, r.evalLabels())
	})

	t.Run("a build that always fails is attempted once per fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) { e.buildErr = func(string, int) error { return errors.New("broken derivation") } })
		r.start()
		waitState(t, r, "failed")
		r.settle(10)
		require.Equal(t, 1, r.exec.buildCount(), "10 ticks without a fetch built again")
		for i := 1; i <= 3; i++ {
			r.fetch()
			require.Eventually(t, func() bool { return r.exec.buildCount() == i+1 }, 15*time.Second, 25*time.Millisecond)
			r.settle(10)
			require.Equal(t, i+1, r.exec.buildCount(), "after fetch %d", i)
			require.Len(t, r.exec.evalLog(), i+1)
		}
		assert.Contains(t, reason(t, r), "build failed "+r.mainHead()+": ")
		assert.Empty(t, r.exec.deployLog())
	})
}

func TestReattemptKeepsTheLazyWalk(t *testing.T) {
	failTesting := func(r *rig, testing string) {
		r.exec.set(func(e *fakeExec) {
			e.evalErr = func(commit string, _ int) error {
				if commit == testing {
					return errors.New("torn HEAD")
				}
				return nil
			}
		})
	}

	t.Run("a failed testing candidate does not block main while it waits for a fetch", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		failTesting(r, testing)
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, []string{"testing", "main"}, r.evalLabels())
		r.exec.setCurrent(oldSystem)
		r.waitDeploys(2)
		r.settle(10)
		assert.Equal(t, []string{"testing", "main"}, r.evalLabels(), "main or testing was evaluated without a fetch")

		r.fetch()
		r.waitEvals(3)
		r.settle(10)
		assert.Equal(t, []string{"testing", "main", "testing"}, r.evalLabels(), "one more evaluation of the failed candidate only")
		assert.Len(t, r.exec.deployLog(), 2)
	})

	t.Run("a failed candidate being evaluated again stops the walk", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		failTesting(r, testing)
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(5)
		r.exec.set(func(e *fakeExec) { e.evalHold = 2 * time.Second })
		r.fetch()
		require.Eventually(t, func() bool { return len(r.exec.evalLog()) == 3 }, 15*time.Second, 25*time.Millisecond)
		v := waitState(t, r, "waiting")
		assert.Equal(t, "evaluating "+testing, reconcileField(t, v, "reason"))
		assert.Equal(t, []string{"pending", "skipped"}, outcomes(t, v))

		r.exec.setCurrent(oldSystem)
		r.settle(3)
		assert.Len(t, r.exec.deployLog(), 1, "main was redeployed while the failed candidate was evaluated")
		assert.Len(t, r.exec.evalLog(), 3)
		r.waitDeploys(2)
		assert.Equal(t, []string{"testing", "main", "testing"}, r.evalLabels())
		waitState(t, r, "converged")
	})
}
