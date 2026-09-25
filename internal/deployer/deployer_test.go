package deployer_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestDeployerBasic(t *testing.T) {
	deployDone := make(chan struct{})
	var deployFunc = func(context.Context, string, string) (bool, string, error) {
		<-deployDone
		return false, "profile-path", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 1, 1)
	assert.Nil(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())
	assert.False(t, d.IsDeploying())

	g := &protobuf.Generation{SelectedCommitId: "commit-1"}
	d.Submit(g, "test")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, d.IsDeploying())
	}, 5*time.Second, 100*time.Millisecond)

	deployDone <- struct{}{}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.IsDeploying())
		assert.Equal(c, "profile-path", d.Deployment().ProfilePath)
	}, 5*time.Second, 100*time.Millisecond)

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		dpl := <-d.DeploymentDoneCh
		assert.Equal(c, "profile-path", dpl.ProfilePath)
		assert.Equal(c, "commit-1", dpl.Generation.SelectedCommitId)
	}, 5*time.Second, 100*time.Millisecond)
}

func TestDeployerSubmit(t *testing.T) {
	deployDone := make(chan struct{})
	var deployFunc = func(context.Context, string, string) (bool, string, error) {
		<-deployDone
		return false, "profile-path", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 1, 1)
	assert.Nil(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())
	assert.False(t, d.IsDeploying())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "test")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, d.IsDeploying())
		assert.Nil(c, d.GenerationToDeploy)
	}, 5*time.Second, 100*time.Millisecond)

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-2"}, "test")
	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-3"}, "test")
	assert.NotNil(t, d.GenerationToDeploy)

	// To simulate the end of 2 deployments (commit-1 and commit-3)
	deployDone <- struct{}{}
	deployDone <- struct{}{}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.IsDeploying())
		assert.Equal(c, "profile-path", d.Deployment().ProfilePath)
		assert.Nil(c, d.GenerationToDeploy)
	}, 5*time.Second, 100*time.Millisecond)

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		dpl := <-d.DeploymentDoneCh
		assert.Equal(c, "profile-path", dpl.ProfilePath)
		assert.Equal(c, "commit-1", dpl.Generation.SelectedCommitId)
	}, 5*time.Second, 100*time.Millisecond)
}

func TestDeployerSuspend(t *testing.T) {
	deployDone := make(chan struct{})
	var deployFunc = func(context.Context, string, string) (bool, string, error) {
		<-deployDone
		return false, "profile-path", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 1, 1)
	assert.Nil(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())
	assert.False(t, d.IsSuspended())
	d.Suspend()
	assert.True(t, d.IsSuspended())
	assert.False(t, d.IsDeploying())
	assert.False(t, d.RunnerIsSuspended())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "test")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, d.RunnerIsSuspended())
	}, 3*time.Second, 100*time.Millisecond)

	d.Resume()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.RunnerIsSuspended())
		assert.True(c, d.IsDeploying())
	}, 3*time.Second, 100*time.Millisecond)
}

// C8 (preserve): an identical generation with the same operation and no
// lease change is not resubmitted. IsAlreadyDeployed compares against
// previousDeployment, which deployer.New sets equal to deployment at
// construction time (i.e. right after a restart, before any new deploy
// starts) - that is the window this dedup guard actually protects: the
// same commit being re-evaluated right after comin restarts must not be
// redeployed. Mutant: force IsAlreadyDeployed to return false.
func TestSubmitSkipsIdenticalGenerationAfterRestart(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 1, 1)
	assert.Nil(t, err)

	g := &protobuf.Generation{SelectedCommitId: "commit-1", SelectedBranchIsTesting: wrapperspb.Bool(false)}
	lastDpl := &protobuf.Deployment{
		Uuid:       "prev",
		Generation: g,
		Operation:  "switch",
		Status:     store.StatusToString(store.Done),
	}

	var deployCount int32
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		atomic.AddInt32(&deployCount, 1)
		return false, "", nil
	}
	// Simulate a comin restart: the deployer is constructed with the
	// last stored deployment, exactly as cmd/run.go does.
	d := deployer.New(s, deployFunc, lastDpl, "")
	d.Run(t.Context())

	// Same generation, same operation, no lease change: not resubmitted.
	// Submit queues synchronously, and the deployer is not idle again
	// before a queued deployment has called deployFunc, so an idle
	// deployer with no call means nothing was queued.
	d.Submit(g, "switch")
	assert.True(t, d.Idle())
	assert.Equal(t, int32(0), atomic.LoadInt32(&deployCount))
}

// C7: switch-latest resolves the generation to deploy at the moment the
// deployer actually starts deploying it, not when SubmitLatest was called.
// Mutant: resolve at submit time.
func TestSubmitLatestResolvesAtDeployTime(t *testing.T) {
	deployDone := make(chan struct{})
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		<-deployDone
		return false, "", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.Nil(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())

	// Occupy the deployer with a slow first deployment.
	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "test")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, d.IsDeploying())
	}, 3*time.Second, 20*time.Millisecond)

	// SubmitLatest while busy: the resolver must not run yet. The
	// deployer is blocked inside deployFunc, so it cannot reach the
	// resolver; only a resolver called by SubmitLatest itself would run.
	var resolveCalls int32
	target := &protobuf.Generation{SelectedCommitId: "commit-latest"}
	d.SubmitLatest(func() (*protobuf.Generation, error) {
		atomic.AddInt32(&resolveCalls, 1)
		return target, nil
	})
	assert.Equal(t, int32(0), atomic.LoadInt32(&resolveCalls))
	_, queued := d.Activity()
	assert.True(t, queued)

	// Let the first deployment finish: only now must the resolver run,
	// and the deployment it produces uses "switch".
	deployDone <- struct{}{}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, int32(1), atomic.LoadInt32(&resolveCalls))
		assert.True(c, d.IsDeploying())
	}, 3*time.Second, 20*time.Millisecond)
	deployDone <- struct{}{}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.IsDeploying())
	}, 3*time.Second, 20*time.Millisecond)

	dpl := d.Deployment()
	assert.Equal(t, "commit-latest", dpl.Generation.SelectedCommitId)
	assert.Equal(t, "switch", dpl.Operation)
	assert.Equal(t, deployer.ReasonDeploymentSwitchLatest, dpl.Reason)
}

// C7: unlike Submit, SubmitLatest is never skipped by IsAlreadyDeployed -
// an explicit switch-latest request always produces a new deployment, even
// of the generation already running. Mutant: route it through
// IsAlreadyDeployed.
func TestSubmitLatestIgnoresIsAlreadyDeployed(t *testing.T) {
	var deployCount int32
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		atomic.AddInt32(&deployCount, 1)
		return false, "", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()

	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.Nil(t, err)
	g := &protobuf.Generation{SelectedCommitId: "commit-1", SelectedBranchIsTesting: wrapperspb.Bool(false)}
	// previousDeployment already matches g (as after a restart, see
	// TestSubmitSkipsIdenticalGenerationAfterRestart): IsAlreadyDeployed(g)
	// reports true here, which is exactly the condition SubmitLatest must
	// ignore.
	lastDpl := &protobuf.Deployment{
		Uuid:       "prev",
		Generation: g,
		Operation:  "switch",
		Status:     store.StatusToString(store.Done),
	}
	d := deployer.New(s, deployFunc, lastDpl, "")
	d.Run(t.Context())
	assert.True(t, d.IsAlreadyDeployed(g), "sanity: this is the exact condition SubmitLatest must not be gated by")

	d.SubmitLatest(func() (*protobuf.Generation, error) { return g, nil })
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, int32(1), atomic.LoadInt32(&deployCount))
	}, 3*time.Second, 20*time.Millisecond)
}
