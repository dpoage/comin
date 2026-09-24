package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
// The fetcher is started, so TriggerFetch runs the whole fetch, evaluate,
// build, confirm and deploy pipeline. A nil newRepository uses a
// RepositoryMock and leaves the fetcher stopped.
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
		fe.Start(t.Context())
	}
	bc := NewConfirmer(bk, Without, 0, "")
	bc.Start()
	dc := NewConfirmer(bk, Without, 0, "")
	dc.Start()
	r.m = New(s, prometheus.New(), scheduler.New(), fe, b, r.d, "", r.exec, bc, dc, bk, rigOperations, lease.NewReader(r.lease), ls)
	r.m.SetPollPeriod(50 * time.Millisecond)
	return r
}

func (r *rig) start(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r.cancel = cancel
	go r.m.Run(ctx)
}

// stop ends this comin process: its manager loop returns, so it no longer
// touches the files a restarted rig shares.
func (r *rig) stop() {
	r.cancel()
	time.Sleep(100 * time.Millisecond)
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

// waitIdle waits until the deployer has been idle for a while, long enough
// for the manager loop to have handled the last finished deployment and
// for several poll ticks to have run.
func (r *rig) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.d.Idle() {
			time.Sleep(200 * time.Millisecond)
			if r.d.Idle() {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the deployer never became idle")
}

func (r *rig) drift() *protobuf.Drift { return r.m.GetState().Drift }

func rebuilt(g *protobuf.Generation) *protobuf.Generation {
	c := proto.CloneOf(g)
	c.Uuid = g.Uuid + "-rebuilt"
	return c
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
	r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r.waitIdle(t)
	assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
	r.waitIdle(t)
	require.True(t, r.leaseExists(), "the hook took a git lease")

	require.NoError(t, os.Remove(r.lease))
	r.waitIdle(t)
	time.Sleep(300 * time.Millisecond) // several more poll periods: still exactly one return
	assert.Equal(t, []string{"m1/switch", "t1/test", "m1/switch"}, r.deployLog())
	assert.Equal(t, "/nix/store/m1", r.exec.get())
	assert.Equal(t, "none", r.drift().State)
}

// Same, with M deployed by an earlier comin process: the restarted
// deployer starts with M as both its current and previous deployment.
func TestC3SingleTestingDeployAfterRestartReturnsOnce(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r1.waitIdle(t)
	r1.stop()

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/m1")
	r2.start(t)
	assert.Equal(t, "submitted:test", r2.fetchDeploy(testingGeneration("t1", "m1")))
	r2.waitIdle(t)
	require.True(t, r2.leaseExists())

	require.NoError(t, os.Remove(r2.lease))
	r2.waitIdle(t)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, []string{"t1/test", "m1/switch"}, r2.deployLog())
	assert.Equal(t, "/nix/store/m1", r2.exec.get())
	assert.Equal(t, "none", r2.drift().State)
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
	r1.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r1.waitIdle(t)
	r1.fetchDeploy(testingGeneration("t1", "m1"))
	r1.waitIdle(t)
	r1.fetchDeploy(testingGeneration("t2", "m1"))
	r1.waitIdle(t)
	require.NoError(t, os.Remove(r1.lease))
	r1.waitIdle(t)
	require.Equal(t, []string{"m1/switch", "t1/test", "t2/test", "m1/switch"}, r1.deployLog())
	r1.stop()

	r2 := newRig(t, dir)
	r2.exec.set("/nix/store/m1")
	r2.start(t)
	assert.Equal(t, "skipped:lease-decision", r2.fetchDeploy(rebuilt(testingGeneration("t2", "m1"))))
	r2.waitIdle(t)
	assert.Empty(t, r2.deployLog())
	assert.False(t, r2.leaseExists())
	assert.Equal(t, "none", r2.drift().State)

	// A new head of the branch still deploys.
	assert.Equal(t, "submitted:test", r2.fetchDeploy(testingGeneration("t3", "m1")))
	r2.waitIdle(t)
	assert.Equal(t, []string{"t3/test"}, r2.deployLog())
}

// ---------------------------------------------------------------------
// C3: a tier commit frozen by the lease deploys once the lease ends
// ---------------------------------------------------------------------

// Mutant: drop the frozen generation instead of deferring it.
func TestC1FrozenTierCommitDeploysAfterLeaseEnds(t *testing.T) {
	t.Run("git override", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitIdle(t)
		r.fetchDeploy(testingGeneration("t1", "m1"))
		r.waitIdle(t)
		require.True(t, r.leaseExists())
		assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
		r.waitIdle(t)
		require.Equal(t, []string{"m1/switch", "t1/test"}, r.deployLog())

		require.NoError(t, os.Remove(r.lease))
		r.waitIdle(t)
		time.Sleep(300 * time.Millisecond)
		// The C3 return to the expected generation, then the frozen
		// tier commit, offered once.
		assert.Equal(t, []string{"m1/switch", "t1/test", "m1/switch", "m2/switch"}, r.deployLog())
		assert.Equal(t, "/nix/store/m2", r.exec.get())
		assert.Equal(t, "none", r.drift().State)
	})
	t.Run("session lease", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitIdle(t)
		r.writeLease(t, "session")
		assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
		r.waitIdle(t)

		require.NoError(t, os.Remove(r.lease))
		r.waitIdle(t)
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, []string{"m1/switch", "m2/switch"}, r.deployLog())
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
	r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r.waitIdle(t)
	r.fetchDeploy(testingGeneration("t1", "m1"))
	r.waitIdle(t)
	require.True(t, r.leaseExists())

	gate := r.holdDeploys()
	r.fetchDeploy(testingGeneration("t1b", "m1"))
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, r.m.SwitchDeploymentLatest()) // persist
	assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t2", "m1")))
	r.releaseDeploys(gate)
	r.waitIdle(t)

	assert.Equal(t, []string{"m1/switch", "t1/test", "t1b/test", "t1b/switch", "t2/test"}, r.deployLog())
	assert.False(t, r.ls.PendingSwitchLatest())
}

// Nothing to switch to: the request fails the way be6025e's did, and
// nothing is left pending for a later restart.
func TestC7SwitchLatestWithEmptyStoreFails(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.start(t)
	err := r.m.SwitchDeploymentLatest()
	assert.EqualError(t, err, "manager: no previous deployment")
	r.waitIdle(t)
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
	r.waitIdle(t)
	assert.Empty(t, r.deployLog())
	assert.False(t, r.ls.PendingSwitchLatest())
	reloaded, err := leasestate.Load(r.lsPath)
	require.NoError(t, err)
	assert.False(t, reloaded.PendingSwitchLatest())
}

// switch-latest is requested while the newer testing deploy is still
// running inside the deploy function (its Status is "running"): the switch
// targets that newer deploy. Mutant: resolve at submit time (it would
// switch t1, the last DONE testing deployment at that moment).
func TestC7SwitchLatestRequestedWhileNewerDeployRunning(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r.waitIdle(t)
	r.fetchDeploy(testingGeneration("t1", "m1"))
	r.waitIdle(t)
	require.True(t, r.leaseExists())

	gate := r.holdDeploys()
	r.fetchDeploy(testingGeneration("t2", "m1"))
	require.Eventually(t, func() bool {
		d := r.d.Deployment()
		return d.Generation.GetSelectedCommitId() == "t2" && d.Status == store.StatusToString(store.Running)
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.releaseDeploys(gate)
	r.waitIdle(t)

	assert.Equal(t, []string{"m1/switch", "t1/test", "t2/test", "t2/switch"}, r.deployLog())
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
	r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r.waitIdle(t)
	r.fetchDeploy(testingGeneration("t1", "m1"))
	r.waitIdle(t)
	require.True(t, r.leaseExists())
	r.failDeploysOf("/nix/store/t2")
	r.fetchDeploy(testingGeneration("t2", "m1"))
	r.waitIdle(t)
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
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	require.NoError(t, os.Remove(r.lease))
	r.waitIdle(t)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done

	assert.Equal(t, []string{"m1/switch", "t1/test", "t2/test", "m1/switch"}, r.deployLog())
	for _, d := range r.s.DeploymentList() {
		if isTestingDeployment(d) {
			assert.True(t, r.ls.IsReleased(d.Uuid), "testing deployment %s released", d.Generation.GetSelectedCommitId())
		}
	}
	statesMu.Lock()
	defer statesMu.Unlock()
	assert.False(t, states["leaseless"], "observed states: %v", states)
	assert.Equal(t, "none", r.drift().State)
}

// ---------------------------------------------------------------------
// C1/C2 re-applied when a queued generation starts
// ---------------------------------------------------------------------

// Mutant: gate only at confirm time.
func TestDeployGateReappliedWhenQueuedGenerationStarts(t *testing.T) {
	t.Run("main queued, lease appears", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitIdle(t)
		gate := r.holdDeploys()
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
		time.Sleep(100 * time.Millisecond)
		// No lease yet: the tier commit passes the gate and queues
		// behind the testing deploy, whose hook then takes the lease.
		assert.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
		r.releaseDeploys(gate)
		r.waitIdle(t)
		assert.True(t, r.leaseExists())
		assert.Equal(t, []string{"m1/switch", "t1/test"}, r.deployLog())
		assert.Equal(t, "held", r.drift().State)
	})
	t.Run("testing queued, session lease appears", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.start(t)
		r.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
		r.waitIdle(t)
		gate := r.holdDeploys()
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t2", "m1")))
		r.writeLease(t, "session")
		r.releaseDeploys(gate)
		r.waitIdle(t)
		assert.Equal(t, []string{"m1/switch", "t1/test"}, r.deployLog())
	})
}

// ---------------------------------------------------------------------
// S4: drift is evaluated without status queries
// ---------------------------------------------------------------------

// Mutant: evaluate drift only when the status is queried.
func TestDriftSinceRecordedWithoutStatusQueries(t *testing.T) {
	t.Run("since within one poll period of the drift start", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mainGeneration("m1", "/nix/store/m1")})
		r.exec.set("/nix/store/m1")
		r.start(t)
		time.Sleep(150 * time.Millisecond)
		start := time.Now()
		r.exec.set("/nix/store/break-glass")
		time.Sleep(500 * time.Millisecond)

		onDisk, err := leasestate.Load(r.lsPath)
		require.NoError(t, err)
		since := onDisk.DriftSince()
		require.NotNil(t, since, "no status query was made, the poll must have recorded the episode")
		assert.WithinDuration(t, start, *since, 2*r.m.pollPeriod)
	})
	t.Run("a new episode gets a new since", func(t *testing.T) {
		r := newRig(t, t.TempDir())
		r.s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mainGeneration("m1", "/nix/store/m1")})
		r.exec.set("/nix/store/break-glass-a")
		r.start(t)
		time.Sleep(300 * time.Millisecond)
		r.exec.set("/nix/store/m1")
		time.Sleep(300 * time.Millisecond)
		startB := time.Now()
		r.exec.set("/nix/store/break-glass-b")
		time.Sleep(500 * time.Millisecond)

		d := r.drift()
		assert.Equal(t, "leaseless", d.State)
		require.NotNil(t, d.Since)
		assert.WithinDuration(t, startB, d.Since.AsTime(), 2*r.m.pollPeriod)
	})
}

// ---------------------------------------------------------------------
// Corrupt lease state: nothing is released (C3(d))
// ---------------------------------------------------------------------

func TestCorruptLeaseStateReleasesNothing(t *testing.T) {
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	r1.fetchDeploy(mainGeneration("m1", "/nix/store/m1"))
	r1.waitIdle(t)
	r1.fetchDeploy(testingGeneration("t1", "m1"))
	r1.waitIdle(t)
	r1.stop()

	content, err := os.ReadFile(r1.lsPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(r1.lsPath, content[:len(content)/2], 0644))
	require.NoError(t, os.Remove(r1.lease))

	r2 := newRig(t, dir)
	require.Error(t, r2.lsErr, "a truncated lease-state file is reported, not silently reset")
	r2.exec.set("/nix/store/t1")
	r2.start(t)
	r2.waitIdle(t)
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, r2.deployLog(), "no lease-aware mark survived, so nothing is released or returned")
	for _, d := range r2.s.DeploymentList() {
		assert.False(t, r2.ls.IsReleased(d.Uuid))
	}
	assert.Equal(t, "leaseless", r2.drift().State)
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
	r.waitIdle(t)

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
	r.waitIdle(t)
	close(stop)
	<-statusDone
}
