package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/lease"
	"github.com/nlewo/comin/internal/leasestate"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// waitDeadline bounds every wait for an observable outcome. It is only
// reached when the outcome never comes.
const waitDeadline = 15 * time.Second

// rig is a Manager wired the way cmd/run.go wires it: the store and the
// lease state are loaded from dir (so a second rig on the same dir is a
// comin restart), the deployer starts from store.LastDeployment(), and the
// post-deployment command stands in for the platform's hook: a done
// deployment of a testing-* branch writes a git lease unless one exists.
// Its deploy function repoints the current system like
// switch-to-configuration does, and can be held on a gate or made to fail.
type rig struct {
	m      *Manager
	d      *deployer.Deployer
	s      *store.Store
	ls     *leasestate.State
	lsErr  error
	exec   *controllableExecutor
	lease  string
	lsPath string
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	gate     chan struct{}
	deployed []string
	failOut  map[string]bool
}

var rigOperations = ConfigurationOperations{"origin": {"main": "switch", "testing-r1": "test"}}

func newRig(t *testing.T, dir string) *rig {
	t.Helper()
	return newRigWithRepository(t, dir, nil)
}

// newRigWithRepository is newRig with the repository newRepository builds
// from the main commit of the last stored deployment, as cmd/run.go does.
// The fetcher is started after the manager is built (as in cmd/run.go),
// so TriggerFetch runs the whole fetch, evaluate, build, confirm and
// deploy pipeline. A nil newRepository uses a RepositoryMock and leaves
// the fetcher stopped.
func newRigWithRepository(t *testing.T, dir string, newRepository func(mainCommitId string) repository.Repository) *rig {
	t.Helper()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(dir, "store.json"), filepath.Join(dir, "gcroots"), 10, 10)
	require.NoError(t, err)
	require.NoError(t, s.Load())
	lsPath := filepath.Join(dir, "lease-state.json")
	// Like cmd/run.go, keep the usable state Load returns even when it
	// reports the file unusable.
	ls, lsErr := leasestate.Load(lsPath)
	r := &rig{s: s, ls: ls, lsErr: lsErr, exec: &controllableExecutor{}, lease: filepath.Join(dir, "override.json"), lsPath: lsPath, failOut: map[string]bool{}}

	var last *protobuf.Deployment
	var mainCommitId string
	if ok, ld := s.LastDeployment(); ok {
		last = ld
		mainCommitId = ld.Generation.GetMainCommitId()
	}
	deployFunc := func(ctx context.Context, outPath, op string) (bool, string, error) {
		r.mu.Lock()
		r.deployed = append(r.deployed, strings.TrimPrefix(outPath, "/nix/store/")+"/"+op)
		gate := r.gate
		fail := r.failOut[outPath]
		r.mu.Unlock()
		r.exec.set(outPath)
		if gate != nil {
			<-gate
		}
		if fail {
			return false, "", fmt.Errorf("switch-to-configuration failed")
		}
		return false, "", nil
	}
	hook := filepath.Join(dir, "hook.sh")
	script := "#!/bin/sh\n" +
		"case \"$COMIN_GIT_REF\" in */testing-*)\n" +
		"  if [ \"$COMIN_STATUS\" = done ] && [ ! -e " + r.lease + " ]; then\n" +
		"    echo '{\"version\":1,\"kind\":\"git\"}' > " + r.lease + "\n" +
		"  fi;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(hook, []byte(script), 0755))
	r.d = deployer.New(s, deployFunc, last, hook)

	b := builder.New(s, r.exec, "", "", "", "host", time.Second, time.Second)
	var fe *fetcher.Fetcher
	if newRepository == nil {
		fe = fetcher.NewFetcher(utils.NewRepositoryMock())
	} else {
		fe = fetcher.NewFetcher(newRepository(mainCommitId))
	}
	bc := NewConfirmer(bk, Without, 0, "")
	bc.Start()
	dc := NewConfirmer(bk, Without, 0, "")
	dc.Start()
	r.m = New(s, prometheus.New(), scheduler.New(), fe, b, r.d, "", r.exec, bc, dc, bk, rigOperations, lease.NewReader(r.lease), ls)
	r.m.SetPollPeriod(50 * time.Millisecond)
	if newRepository != nil {
		fe.Start(t.Context())
	}
	// Registered after the test's TempDir, so it runs before the
	// directory is removed: nothing of this comin process may still be
	// writing into it by then.
	t.Cleanup(func() {
		r.stop()
		waitFor(t, func() bool {
			return deployerQuiet(r.d) && !fe.IsFetching() && !b.IsEvaluating() && !b.State().IsBuilding.GetValue()
		}, "comin is quiescent")
	})
	return r
}

func (r *rig) start(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		r.m.Run(ctx)
	}()
}

// stop ends this comin process: once it returns, its manager loop has
// returned, so it no longer touches the files a restarted rig shares.
func (r *rig) stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

func (r *rig) holdDeploys() chan struct{} {
	gate := make(chan struct{})
	r.mu.Lock()
	r.gate = gate
	r.mu.Unlock()
	return gate
}

func (r *rig) releaseDeploys(gate chan struct{}) {
	r.mu.Lock()
	r.gate = nil
	r.mu.Unlock()
	close(gate)
}

func (r *rig) failDeploysOf(outPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOut[outPath] = true
}

func (r *rig) deployLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deployed...)
}

func (r *rig) writeLease(t *testing.T, kind string) {
	require.NoError(t, os.WriteFile(r.lease, []byte(`{"version":1,"kind":"`+kind+`"}`), 0644))
}

func (r *rig) leaseExists() bool {
	_, err := os.Stat(r.lease)
	return err == nil
}

// fetchDeploy is FetchAndBuild's tail for a built generation: the
// IsAlreadyDeployed filter, the deploy gate, then Submit.
func (r *rig) fetchDeploy(g *protobuf.Generation) string {
	if r.d.IsAlreadyDeployed(g) {
		return "skipped:already-deployed"
	}
	op, ok := r.m.leaseDeployDecision(g)
	if !ok {
		return "skipped:lease-decision"
	}
	r.d.Submit(g, op)
	return "submitted:" + op
}

func waitFor(t *testing.T, cond func() bool, msgAndArgs ...any) {
	t.Helper()
	require.Eventually(t, cond, waitDeadline, 5*time.Millisecond, msgAndArgs...)
}

// processed reports whether the manager loop has handled the deployer's
// current deployment: it is the latest store entry, and a testing one is
// marked lease-aware.
func (r *rig) processed() bool {
	cur := r.d.Deployment()
	if cur == nil {
		return true
	}
	ok, last := r.s.LastDeployment()
	if !ok || last.Uuid != cur.Uuid {
		return false
	}
	return !isTestingDeployment(cur) || r.ls.IsLeaseAware(cur.Uuid)
}

// waitDeploys waits until the deploy log is exactly want, nothing is
// queued or in flight, and the manager loop has handled the last
// deployment. A log that grows past want never matches, so an extra
// deployment fails the wait.
func (r *rig) waitDeploys(t *testing.T, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	ok := assert.Eventually(t, func() bool {
		return slices.Equal(r.deployLog(), want) && r.d.Idle() && r.processed()
	}, waitDeadline, 5*time.Millisecond)
	if !ok {
		t.Fatalf("deploy log %v, want %v (idle=%v)", r.deployLog(), want, r.d.Idle())
	}
}

// waitDrift waits until nothing is queued or in flight and S4 reports
// state.
func (r *rig) waitDrift(t *testing.T, state string) {
	t.Helper()
	ok := assert.Eventually(t, func() bool {
		return r.d.Idle() && r.drift().State == state
	}, waitDeadline, 5*time.Millisecond)
	if !ok {
		t.Fatalf("drift %+v, want state %s (deploys %v)", r.drift(), state, r.deployLog())
	}
}

func (r *rig) drift() *protobuf.Drift { return r.m.GetState().Drift }

func rebuilt(g *protobuf.Generation) *protobuf.Generation {
	c := proto.CloneOf(g)
	c.Uuid = g.Uuid + "-rebuilt"
	return c
}

// overrideOn deploys m1, then n testing heads t1..tn on it. The hook takes
// the git lease after the first one.
func (r *rig) overrideOn(t *testing.T, n int) {
	t.Helper()
	r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	want := []string{"m1/switch"}
	r.waitDeploys(t, want...)
	for i := 1; i <= n; i++ {
		c := fmt.Sprintf("t%d", i)
		require.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration(c, "m1")))
		want = append(want, c+"/test")
		r.waitDeploys(t, want...)
	}
	require.True(t, r.leaseExists(), "the hook took a git lease")
}

// ---------------------------------------------------------------------
// C3: the return is not filtered by IsAlreadyDeployed
// ---------------------------------------------------------------------

// One testing deploy, then the operator ends the lease. The deployer's
// previous deployment is M, so a return through Submit would be skipped
// as "already deployed". Mutant: return via plain Submit.
func TestC3SingleTestingDeployOperatorEndReturnsOnce(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "t1/test", "m1/switch")
	r.waitDrift(t, "none")
	assert.Equal(t, "/nix/store/m1", r.exec.get())
}

// Same, with M deployed by an earlier comin process: the restarted
// deployer starts with M as both its current and previous deployment.
func TestC3SingleTestingDeployAfterRestartReturnsOnce(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r1.waitDeploys(t, "m1/switch")
	r1.stop()

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/m1")
	r2.start(t)
	assert.Equal(t, "submitted:test", r2.fetchDeploy(testingGeneration("t1", "m1")))
	r2.waitDeploys(t, "t1/test")
	require.True(t, r2.leaseExists())

	require.NoError(t, os.Remove(r2.lease))
	r2.waitDeploys(t, "t1/test", "m1/switch")
	r2.waitDrift(t, "none")
	assert.Equal(t, "/nix/store/m1", r2.exec.get())
}

// ---------------------------------------------------------------------
// P-C3-ONCE: one return decision per ended override
// ---------------------------------------------------------------------

// A git lease ends by reboot: the machine boots M, the lease is gone, and
// comin restarts. C3 decides once (nothing to return). A later
// out-of-band change is leaseless drift, never reverted, including after
// another restart. Mutant: no consumed check.
func TestC3OnceRebootEndedLeaseThenOutOfBandChange(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			dir := t.TempDir()
			r1 := newRig(t, dir)
			r1.start(t)
			r1.overrideOn(t, n)
			r1.stop()
			require.NoError(t, os.Remove(r1.lease))

			r2 := newRig(t, dir)
			r2.exec.set("/nix/store/m1")
			r2.start(t)
			last := r2.d.Deployment()
			waitFor(t, func() bool { return r2.ls.IsReleased(last.Uuid) }, "C3 decided")
			r2.waitDrift(t, "none")

			r2.exec.set("/nix/store/break-glass")
			r2.waitDrift(t, "leaseless")
			assert.Empty(t, r2.deployLog())
			r2.stop()

			r3 := newRig(t, dir)
			r3.exec.set("/nix/store/break-glass")
			r3.start(t)
			r3.waitDrift(t, "leaseless")
			assert.Empty(t, r3.deployLog())
		})
	}
}

// A git lease ends by reboot, later a closure override is applied out of
// band and ends. Whether to return a closure is the lease tool's
// decision (it calls switch-latest), never C3's. Mutant: no consumed
// check.
func TestC3OnceClosureLeaseAfterRebootEndedGitLease(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.overrideOn(t, 1)
	r1.stop()
	require.NoError(t, os.Remove(r1.lease))

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/m1")
	r2.start(t)
	last := r2.d.Deployment()
	waitFor(t, func() bool { return r2.ls.IsReleased(last.Uuid) }, "C3 decided")
	r2.waitDrift(t, "none")

	r2.writeLease(t, "closure")
	r2.exec.set("/nix/store/closure-c")
	r2.waitDrift(t, "held")
	require.NoError(t, os.Remove(r2.lease))
	r2.waitDrift(t, "leaseless")
	assert.Empty(t, r2.deployLog())
	assert.Equal(t, "/nix/store/closure-c", r2.exec.get())
}

// comin restarts after C3 released the override but before the return
// deployed: the decision is on disk, so the restarted comin returns
// exactly once. Mutant: the return is not recorded with the release.
func TestC3DecisionSurvivesRestartBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.overrideOn(t, 1)
	x := r1.d.Deployment()

	// The deployer takes nothing more, so the return stays queued.
	r1.d.Suspend()
	require.NoError(t, os.Remove(r1.lease))
	waitFor(t, func() bool {
		onDisk, err := leasestate.Load(r1.lsPath)
		return err == nil && onDisk.IsReleased(x.Uuid)
	}, "C3 decided")
	r1.stop()
	require.Equal(t, []string{"m1/switch", "t1/test"}, r1.deployLog())

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/t1")
	r2.start(t)
	r2.waitDeploys(t, "m1/switch")
	r2.waitDrift(t, "none")
	assert.False(t, r2.ls.PendingSwitchLatest())
}

// Ruling 1: a system changed out of band while the git lease was held is
// not comin's; when the lease ends C3 records its decision without a
// return. Mutant: no guard.
func TestC3BreakGlassDuringGitLeaseIsNotReturned(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)
	x := r.d.Deployment()

	r.exec.set("/nix/store/break-glass")
	require.NoError(t, os.Remove(r.lease))
	r.waitDrift(t, "leaseless")
	assert.True(t, r.ls.IsReleased(x.Uuid))
	assert.Equal(t, []string{"m1/switch", "t1/test"}, r.deployLog())
	assert.Equal(t, "/nix/store/break-glass", r.exec.get())
}

// A tier commit fetched after the override ended but before C3 decided
// is deferred, not deployed ahead of the return: deploying it first would
// leave the override unreleased and its testing head selectable again.
// With an hour-long poll period, only the poll the manager runs after
// each deployment and the test's own poll run C3. Once the return has
// finished, the deferred commit is offered. Mutant: the gate admits main
// generations as soon as the lease file is gone.
func TestMainFetchedBeforeC3DecidesIsDeferred(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.m.SetPollPeriod(time.Hour)
	r.start(t)
	r.overrideOn(t, 1)
	x := r.d.Deployment()
	// The poll after t1's deployment recorded the held episode: it has
	// finished, so no poll runs until the test's.
	waitFor(t, func() bool { return r.ls.DriftSince() != nil }, "the post-deployment poll ran")

	require.NoError(t, os.Remove(r.lease))
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
	assert.False(t, r.ls.IsReleased(x.Uuid))
	r.m.poll(time.Now().UTC())
	r.waitDeploys(t, "m1/switch", "t1/test", "m1/switch", "m2/switch")
	assert.True(t, r.ls.IsReleased(x.Uuid))
}

// ---------------------------------------------------------------------
// C3: a released head is never redeployed
// ---------------------------------------------------------------------

// Return, restart, the fetcher re-emits the released head (its selected
// commit starts empty after a restart). Mutant: no released check in the
// deploy gate.
func TestC3ReleasedHeadNotRedeployedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.overrideOn(t, 2)
	require.NoError(t, os.Remove(r1.lease))
	r1.waitDeploys(t, "m1/switch", "t1/test", "t2/test", "m1/switch")
	r1.stop()

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/m1")
	r2.start(t)
	assert.Equal(t, "skipped:lease-decision", r2.fetchDeploy(rebuilt(testingGeneration("t2", "m1"))))
	r2.waitDrift(t, "none")
	assert.Empty(t, r2.deployLog())
	assert.False(t, r2.leaseExists())

	// A new head of the branch still deploys.
	assert.Equal(t, "submitted:test", r2.fetchDeploy(testingGeneration("t3", "m1")))
	r2.waitDeploys(t, "t3/test")
}

// ---------------------------------------------------------------------
// C1: a tier commit frozen by the lease deploys once the lease ends
// ---------------------------------------------------------------------

// Mutant: drop the frozen generation instead of deferring it.
func TestC1FrozenTierCommitDeploysAfterLeaseEnds(t *testing.T) {
	t.Run("git override", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.overrideOn(t, 1)
		assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))

		require.NoError(t, os.Remove(r.lease))
		// The C3 return to the expected generation, then the frozen
		// tier commit, offered once.
		r.waitDeploys(t, "m1/switch", "t1/test", "m1/switch", "m2/switch")
		r.waitDrift(t, "none")
		assert.Equal(t, "/nix/store/m2", r.exec.get())
	})
	t.Run("session lease", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitDeploys(t, "m1/switch")
		r.writeLease(t, "session")
		assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))

		require.NoError(t, os.Remove(r.lease))
		r.waitDeploys(t, "m1/switch", "m2/switch")
		r.waitDrift(t, "none")
	})
}

// ---------------------------------------------------------------------
// C7: switch-latest
// ---------------------------------------------------------------------

// A switch-latest request is queued behind an in-flight deploy, then a
// fetched generation is submitted. Both deploy: the switch first.
// Mutant: Submit clears the queued switch-latest resolver.
func TestC7SwitchLatestSurvivesALaterSubmit(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)

	gate := r.holdDeploys()
	r.fetchDeploy(testingGeneration("t1b", "m1"))
	waitFor(t, func() bool { return slices.Contains(r.deployLog(), "t1b/test") }, "t1b in flight")
	require.NoError(t, r.m.SwitchDeploymentLatest()) // persist
	assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t2", "m1")))
	r.releaseDeploys(gate)

	r.waitDeploys(t, "m1/switch", "t1/test", "t1b/test", "t1b/switch", "t2/test")
	assert.False(t, r.ls.PendingSwitchLatest())
}

// Nothing to switch to: the request fails the way be6025e's did, and
// nothing is left pending for a later restart.
func TestC7SwitchLatestWithEmptyStoreFails(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	err := r.m.SwitchDeploymentLatest()
	assert.EqualError(t, err, "manager: no previous deployment")
	assert.True(t, r.d.Idle())
	assert.Empty(t, r.deployLog())
	reloaded, err := leasestate.Load(r.lsPath)
	require.NoError(t, err)
	assert.False(t, reloaded.PendingSwitchLatest())
}

// A pending request re-armed at restart that resolves to nothing is
// settled: the flag is cleared instead of firing again at every restart.
// Mutant: clear the pending flag only when resolution succeeds.
func TestC7PendingSwitchLatestClearedWhenResolutionFails(t *testing.T) {
	dir := t.TempDir()
	ls, err := leasestate.Load(filepath.Join(dir, "lease-state.json"))
	require.NoError(t, err)
	require.NoError(t, ls.SetPendingSwitchLatest(true))

	r := newRig(t, dir)
	require.True(t, r.ls.PendingSwitchLatest())
	r.start(t)
	waitFor(t, func() bool {
		onDisk, err := leasestate.Load(r.lsPath)
		return err == nil && !onDisk.PendingSwitchLatest() && r.d.Idle()
	}, "the pending request was settled")
	assert.Empty(t, r.deployLog())
}

// switch-latest is requested while the newer testing deploy is still
// running inside the deploy function (its Status is "running"): the switch
// targets that newer deploy. Mutant: resolve at submit time (it would
// switch t1, the last DONE testing deployment at that moment).
func TestC7SwitchLatestRequestedWhileNewerDeployRunning(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)

	gate := r.holdDeploys()
	r.fetchDeploy(testingGeneration("t2", "m1"))
	waitFor(t, func() bool {
		d := r.d.Deployment()
		return d.Generation.GetSelectedCommitId() == "t2" && d.Status == store.StatusToString(store.Running)
	})
	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.releaseDeploys(gate)

	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test", "t2/switch")
}

// ---------------------------------------------------------------------
// C3: a failed current testing deployment still returns
// ---------------------------------------------------------------------

// t1 done, the extend t2 fails activation (current system already
// repointed), the operator ends the lease. Mutant: C3 only for a Done
// current deployment.
func TestC3ReturnsWhenCurrentTestingDeployFailed(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)
	r.failDeploysOf("/nix/store/t2")
	r.fetchDeploy(testingGeneration("t2", "m1"))
	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test")
	require.Equal(t, store.StatusToString(store.Failed), r.d.Deployment().Status)

	states := map[string]bool{}
	var statesMu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				s := r.drift().State
				statesMu.Lock()
				states[s] = true
				statesMu.Unlock()
			}
		}
	}()
	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test", "m1/switch")
	r.waitDrift(t, "none")
	close(stop)
	<-done

	for _, d := range r.s.DeploymentList() {
		if isTestingDeployment(d) {
			assert.True(t, r.ls.IsReleased(d.Uuid), "testing deployment %s released", d.Generation.GetSelectedCommitId())
		}
	}
	statesMu.Lock()
	defer statesMu.Unlock()
	assert.False(t, states["leaseless"], "observed states: %v", states)
}

// ---------------------------------------------------------------------
// C1/C2 re-applied when a queued generation starts
// ---------------------------------------------------------------------

// The queued generation is refused when the deployer takes it, so the
// deployer only becomes idle with the log unchanged. Mutant: gate only at
// confirm time.
func TestDeployGateReappliedWhenQueuedGenerationStarts(t *testing.T) {
	t.Run("main queued, lease appears", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitDeploys(t, "m1/switch")
		gate := r.holdDeploys()
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
		waitFor(t, func() bool { return slices.Contains(r.deployLog(), "t1/test") }, "t1 in flight")
		// No lease yet: the tier commit passes the gate and queues
		// behind the testing deploy, whose hook then takes the lease.
		assert.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
		r.releaseDeploys(gate)
		r.waitDeploys(t, "m1/switch", "t1/test")
		assert.True(t, r.leaseExists())
		r.waitDrift(t, "held")
	})
	t.Run("testing queued, session lease appears", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitDeploys(t, "m1/switch")
		gate := r.holdDeploys()
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
		waitFor(t, func() bool { return slices.Contains(r.deployLog(), "t1/test") }, "t1 in flight")
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t2", "m1")))
		r.writeLease(t, "session")
		r.releaseDeploys(gate)
		r.waitDeploys(t, "m1/switch", "t1/test")
	})
}

// ---------------------------------------------------------------------
// S4: drift is evaluated without status queries
// ---------------------------------------------------------------------

// Past the status query newDriftRig makes (Run serves it only once its
// startup poll is over), nothing queries the status: only the test's
// polls can record or clear `since`, stamped with the poll's time. The
// poll period is an hour, so no other poll runs. Mutant: evaluate drift
// only when the status is queried.
func TestDriftSinceRecordedWithoutStatusQueries(t *testing.T) {
	sinceOnDisk := func(r *rig) *time.Time {
		onDisk, err := leasestate.Load(r.lsPath)
		if err != nil {
			return nil
		}
		return onDisk.DriftSince()
	}
	newDriftRig := func(t *testing.T, current string) *rig {
		r := newRig(t, t.TempDir())
		r.m.SetPollPeriod(time.Hour)
		r.s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mainGeneration("m1", "/nix/store/m1")})
		r.exec.set(current)
		r.start(t)
		r.drift()
		return r
	}
	t.Run("since is the time of the poll that saw the drift", func(t *testing.T) {
		r := newDriftRig(t, "/nix/store/m1")
		require.Nil(t, sinceOnDisk(r))
		r.exec.set("/nix/store/break-glass")
		at := time.Now().UTC()
		r.m.poll(at)
		since := sinceOnDisk(r)
		require.NotNil(t, since, "the poll recorded the episode")
		assert.True(t, since.Equal(at), "since %s, want %s", since, at)
	})
	t.Run("a new episode gets a new since", func(t *testing.T) {
		r := newDriftRig(t, "/nix/store/break-glass-a")
		require.NotNil(t, sinceOnDisk(r), "episode A recorded")
		r.exec.set("/nix/store/m1")
		r.m.poll(time.Now().UTC())
		require.Nil(t, sinceOnDisk(r), "episode A is over")
		r.exec.set("/nix/store/break-glass-b")
		atB := time.Now().UTC()
		r.m.poll(atB)
		since := sinceOnDisk(r)
		require.NotNil(t, since, "episode B recorded")
		assert.True(t, since.Equal(atB), "since %s, want %s", since, atB)
	})
}

// ---------------------------------------------------------------------
// Corrupt lease state
// ---------------------------------------------------------------------

// A truncated lease-state file loses every mark. Nothing is released or
// returned (no deployment counts as lease-aware), and every testing head
// deployed before the loss counts as ended: the deploy gate refuses it
// and the fetcher's selection excludes it. (The deployer itself would
// skip t1 here as its previous deployment, so the gate is asked
// directly.) Mutant: silent reset.
func TestCorruptLeaseStateReleasesNothing(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.overrideOn(t, 1)
	r1.stop()

	content, err := os.ReadFile(r1.lsPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(r1.lsPath, content[:len(content)/2], 0644))
	require.NoError(t, os.Remove(r1.lease))

	r2 := newRig(t, dir)
	require.Error(t, r2.lsErr, "a truncated lease-state file is reported, not silently reset")
	r2.exec.set("/nix/store/t1")
	r2.start(t)
	r2.waitDrift(t, "leaseless")
	assert.Empty(t, r2.deployLog(), "no lease-aware mark survived, so nothing is released or returned")
	for _, d := range r2.s.DeploymentList() {
		assert.False(t, r2.ls.IsReleased(d.Uuid))
	}
	_, ok := r2.m.leaseDeployDecision(rebuilt(testingGeneration("t1", "m1")))
	assert.False(t, ok, "the gate refuses a testing head deployed before the loss")
	assert.True(t, r2.m.testingSelection().Excluded[testingHead(testingGeneration("t1", "m1"))], "the selection excludes it")

	// The loss is recorded, so it still holds after another restart.
	reloaded, err := leasestate.Load(r2.lsPath)
	require.NoError(t, err)
	assert.NotNil(t, reloaded.MarksLostAt())
}

// ---------------------------------------------------------------------
// Races: the production goroutines, run under `go test -race`
// ---------------------------------------------------------------------

// The manager loop, the deployer, status queries marshaled the way the
// gRPC server does, switch-latest requests and fetched submissions, all at
// once. The assertion that matters is the race detector's.
func TestProductionGoroutinesAreRaceFree(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.fetchDeploy(mainGeneration("m0", "/nix/store/m0"))
	r.waitDeploys(t, "m0/switch")

	stop := make(chan struct{})
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		for {
			select {
			case <-stop:
				return
			default:
				body := marshalStatus(t, r.m.GetState())
				assert.Contains(t, body, `"drift":{`)
			}
		}
	}()
	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		for i := 0; i < 20; i++ {
			assert.NoError(t, r.m.SwitchDeploymentLatest())
			time.Sleep(15 * time.Millisecond)
		}
	}()
	for i := 0; i < 15; i++ {
		c := fmt.Sprintf("m%c", 'a'+i)
		r.fetchDeploy(mainGeneration(c, "/nix/store/"+c))
		time.Sleep(10 * time.Millisecond)
	}
	<-switchDone
	waitFor(t, func() bool { return r.d.Idle() && r.processed() })
	close(stop)
	<-statusDone
}

// ---------------------------------------------------------------------
// overrideLeaseFile null: be6025e's testing selection
// ---------------------------------------------------------------------

// Ruling 2: with the lease reader disabled, the fetcher's testing
// selection is the default one, whatever the lease state holds. Mutant:
// build the selection without checking that the feature is enabled.
func TestNullLeaseFileKeepsDefaultTestingSelection(t *testing.T) {
	f := newLeaseFixture(t, nil)
	f.store.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mainGeneration("m1", "/nix/store/m1")})
	f.store.DeploymentInsert(&protobuf.Deployment{Uuid: "t1-dpl", Operation: "test", Status: store.StatusToString(store.Done), Generation: testingGeneration("t1", "m1")})
	require.NoError(t, f.leaseState.MarkLeaseAware("t1-dpl"))
	require.NoError(t, f.leaseState.Release([]string{"t1-dpl"}, false))
	f.writeLease(t, "git")

	enabled := f.testingSelection()
	require.Equal(t, "m1", enabled.Base, "sanity: with the feature on, the selection is not the default")
	require.NotEmpty(t, enabled.Excluded)

	f.Manager.leaseReader = lease.NewReader("")
	assert.Equal(t, repository.TestingSelection{}, f.testingSelection())
}
