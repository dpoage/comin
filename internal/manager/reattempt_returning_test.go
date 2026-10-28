package manager

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pointTestingAt makes the testing branch select commit again.
func (r *rig) pointTestingAt(commit string) {
	r.t.Helper()
	require.NoError(r.t, r.remote.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("testing"), plumbing.NewHash(commit))))
}

func TestAReturningCandidateKeepsItsGeneration(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.start()
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))

	testing := r.pushTesting("file-t1")
	r.exec.set(func(e *fakeExec) { e.buildDelay = 2 * time.Second })
	r.fetch()
	r.waitEvals(2)
	r.waitBuilds(2)
	time.Sleep(300 * time.Millisecond)
	r.resetTestingToMain()
	r.fetch()
	time.Sleep(2 * time.Second)

	r.pointTestingAt(testing)
	r.fetch()
	r.waitDeploys(2)
	r.waitCurrent(r.out(testing))
	r.settle(10)
	assert.Equal(t, []string{"main", "testing"}, r.evalLabels(), "the returning candidate was evaluated again")
	assert.Len(t, r.exec.deployLog(), 2)
}
