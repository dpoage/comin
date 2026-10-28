package manager

import (
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failEvalOnce fails only the first evaluation of commit.
func failEvalOnce(commit string) func(string, int) error {
	return func(c string, attempt int) error {
		if c == commit && attempt == 1 {
			return errors.New("torn HEAD")
		}
		return nil
	}
}

// resetTestingToMain points the testing branch back at main's head.
func (r *rig) resetTestingToMain() {
	r.t.Helper()
	main, err := r.remote.Reference(plumbing.NewBranchReferenceName("main"), false)
	require.NoError(r.t, err)
	require.NoError(r.t, r.remote.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("testing"), main.Hash())))
}

func TestAVanishedCandidateNeverHoldsAReattempt(t *testing.T) {
	t.Run("a candidate that vanishes while its evaluation runs", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		main := r.mainHead()
		r.exec.set(func(e *fakeExec) { e.evalErr = failEvalOnce(main) })
		r.start()
		waitState(t, r, "failed")

		r.pushTesting("file-t1")
		r.exec.set(func(e *fakeExec) { e.evalHold = 2 * time.Second })
		r.fetch()
		r.waitEvals(2)
		r.resetTestingToMain()
		r.fetch()

		r.waitDeploys(1)
		r.waitCurrent(r.out(main))
		r.settle(10)
		assert.Equal(t, []string{"main", "testing", "main"}, r.evalLabels())
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("a candidate that vanishes while awaiting its build confirmation", func(t *testing.T) {
		r := newRig(t, rigOpts{manualBuild: true})
		main := r.mainHead()
		r.exec.set(func(e *fakeExec) { e.evalErr = failEvalOnce(main) })
		r.start()
		waitState(t, r, "failed")

		r.pushTesting("file-t1")
		r.fetch()
		var stale string
		require.Eventually(t, func() bool {
			stale = r.m.BuildConfirmer.status().Submitted
			return stale != ""
		}, 15*time.Second, 25*time.Millisecond, "no testing build waits on the confirmer")

		r.resetTestingToMain()
		r.fetch()
		var uuid string
		require.Eventually(t, func() bool {
			uuid = r.m.BuildConfirmer.status().Submitted
			return uuid != "" && uuid != stale
		}, 15*time.Second, 25*time.Millisecond, "the failed main was not re-attempted")
		assert.Equal(t, []string{"main", "testing", "main"}, r.evalLabels())

		r.m.BuildConfirmer.Confirm(uuid)
		r.waitDeploys(1)
		r.waitCurrent(r.out(main))
		r.settle(10)
		assert.Equal(t, 2, countEval(r.evalLabels(), "main"))
		assert.Len(t, r.exec.deployLog(), 1)
	})
}
