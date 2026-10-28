package manager

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countEval(labels []string, branch string) int {
	n := 0
	for _, l := range labels {
		if l == branch {
			n++
		}
	}
	return n
}

// failEvalFor fails every evaluation of commit, failBuildFor every build of it.
func failEvalFor(commit string) func(string, int) error {
	return func(c string, _ int) error {
		if c == commit {
			return errors.New("torn HEAD")
		}
		return nil
	}
}

func failBuildFor(commit string) func(string, int) error {
	return func(drv string, _ int) error {
		if strings.HasSuffix(drv, commit+".drv") {
			return errors.New("substituter down")
		}
		return nil
	}
}

func (r *rig) waitBuilds(n int) {
	r.t.Helper()
	require.Eventually(r.t, func() bool { return r.exec.buildCount() >= n }, 15*time.Second, 25*time.Millisecond, "builds: %d", r.exec.buildCount())
}

// streamFetches fetches every d until the returned stop is called.
func (r *rig) streamFetches(d time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.fetch()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func TestAReattemptWaitsForTheBuilder(t *testing.T) {
	t.Run("a fetch during a lower candidate's build", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) {
			e.evalErr = failEvalFor(testing)
			e.buildDelay = 3 * time.Second
		})
		r.start()
		r.waitEvals(2)
		r.waitBuilds(1)
		time.Sleep(500 * time.Millisecond)
		require.Equal(t, []string{"testing", "main"}, r.evalLabels())

		r.fetch()
		time.Sleep(time.Second)
		assert.Equal(t, []string{"testing", "main"}, r.evalLabels(), "the re-attempt evaluated during main's build")
		assert.Equal(t, 1, r.exec.buildCount(), "the re-attempt restarted main's build")

		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 1, countEval(r.evalLabels(), "main"), "main was evaluated again")
		assert.Equal(t, 1, r.exec.buildCount())
		assert.Len(t, r.exec.deployLog(), 1)
		assert.Equal(t, 2, countEval(r.evalLabels(), "testing"), "the fetch is owed exactly one re-attempt")
	})

	t.Run("fetches quicker than a lower candidate's build", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) {
			e.evalErr = failEvalFor(testing)
			e.buildDelay = 1500 * time.Millisecond
		})
		r.start()
		r.waitEvals(2)
		r.waitBuilds(1)
		stop := r.streamFetches(600 * time.Millisecond)
		r.waitDeploys(1)
		stop()
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 1, countEval(r.evalLabels(), "main"), "main was evaluated again")
		assert.Equal(t, 1, r.exec.buildCount(), "main's build was restarted")
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("a fetch does not replace a lower candidate's pending build confirmation", func(t *testing.T) {
		r := newRig(t, rigOpts{manualBuild: true})
		testing := r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) { e.evalErr = failEvalFor(testing) })
		r.start()
		var uuid string
		require.Eventually(t, func() bool {
			uuid = r.m.BuildConfirmer.status().Submitted
			return uuid != ""
		}, 15*time.Second, 25*time.Millisecond, "no build waits on the confirmer")
		require.Equal(t, []string{"testing", "main"}, r.evalLabels())

		r.fetch()
		r.settle(10)
		assert.Equal(t, uuid, r.m.BuildConfirmer.status().Submitted, "the re-attempt replaced main's pending build confirmation")
		assert.Equal(t, []string{"testing", "main"}, r.evalLabels(), "the re-attempt re-evaluated a candidate")

		r.m.BuildConfirmer.Confirm(uuid)
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 1, countEval(r.evalLabels(), "main"))
		assert.Equal(t, 1, r.exec.buildCount())
		assert.Len(t, r.exec.deployLog(), 1)
		assert.Equal(t, 2, countEval(r.evalLabels(), "testing"))
	})
}

func TestTheFailureLandsInTheCurrentEpoch(t *testing.T) {
	t.Run("a fetch during a failing evaluation is covered by it", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) {
			e.evalErr = func(string, int) error { return errors.New("torn HEAD") }
			e.evalHold = time.Second
		})
		r.start()
		r.waitEvals(1)
		time.Sleep(300 * time.Millisecond)
		r.fetch()
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "eval failed "+r.mainHead())
		assert.Len(t, r.exec.evalLog(), 1, "a fetch completing during the evaluation re-attempted it")
		r.settle(10)
		assert.Len(t, r.exec.evalLog(), 1)

		r.fetch()
		r.waitEvals(2)
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "eval failed "+r.mainHead())
	})

	t.Run("a fetch during a failing build is covered by it", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.exec.set(func(e *fakeExec) {
			e.buildErr = func(string, int) error { return errors.New("broken derivation") }
			e.buildDelay = 1500 * time.Millisecond
		})
		r.start()
		r.waitBuilds(1)
		time.Sleep(300 * time.Millisecond)
		r.fetch()
		waitState(t, r, "failed")
		assert.Contains(t, reason(t, r), "build failed "+r.mainHead())
		assert.Equal(t, 1, r.exec.buildCount(), "a fetch completing during the build re-attempted it")
		assert.Len(t, r.exec.evalLog(), 1)
		r.settle(10)
		assert.Equal(t, 1, r.exec.buildCount())

		r.fetch()
		r.waitBuilds(2)
		waitState(t, r, "failed")
	})
}

func TestAFailingCandidateSlowerThanTheFetchesDoesNotBlockMain(t *testing.T) {
	t.Run("an evaluation slower than the fetch period", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) {
			e.evalErr = failEvalFor(testing)
			e.evalHold = time.Second
		})
		r.start()
		stop := r.streamFetches(600 * time.Millisecond)
		r.waitDeploys(1)
		stop()
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 1, countEval(r.evalLabels(), "main"), "main was evaluated again")
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("a build slower than the fetch period", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		testing := r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) {
			e.buildErr = failBuildFor(testing)
			e.buildDelay = 1500 * time.Millisecond
		})
		r.start()
		stop := r.streamFetches(600 * time.Millisecond)
		r.waitDeploys(1)
		stop()
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(10)
		assert.Equal(t, 1, countEval(r.evalLabels(), "main"), "main was evaluated again")
		assert.Len(t, r.exec.deployLog(), 1)
	})
}
