package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/leasestate"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests restart comin over a store whose lease-state.json is gone
// (never written by a pre-fork comin, or moved aside) and drive the
// whole fetch through the real repository package:
// selection, build, deploy gate and deployer.

// newGitRigRunning is newGitRig on a machine that runs the system current
// (the path of the commit it runs, "" for none) when comin starts.
func newGitRigRunning(t *testing.T, dir string, remote *gitRemote, current string) *gitRig {
	cfg := types.GitConfig{
		Path: filepath.Join(dir, "repository"),
		Remotes: []types.Remote{{
			Name:    "origin",
			URL:     remote.dir,
			Timeout: 30,
			Branches: types.Branches{
				Main:    types.Branch{Name: "main"},
				Testing: types.Branch{Name: "testing-r1"},
			},
		}},
	}
	r := newRigRunning(t, dir, current, func(mainCommitId string) repository.Repository {
		repo, err := repository.New(cfg, mainCommitId, prometheus.New())
		require.NoError(t, err)
		return repo
	})
	return &gitRig{rig: r, remote: remote}
}

// fetchSelected runs one poll of the remote and returns the name of the
// commit the repository selected, once its generation is built.
func (r *gitRig) fetchSelected(t *testing.T) string {
	t.Helper()
	before := map[string]bool{}
	for _, g := range r.s.GetState().Generations {
		before[g.Uuid] = true
	}
	r.m.Fetcher.TriggerFetch([]string{"origin"})
	var selected string
	waitFor(t, func() bool {
		for _, g := range r.s.GetState().Generations {
			if !before[g.Uuid] && g.BuildStatus == store.Built.String() {
				selected = r.remote.names[g.SelectedCommitId]
				return true
			}
		}
		return false
	}, "the fetch built nothing")
	return selected
}

// settled waits for the poll's evaluate, build and deploy pipeline to be
// quiet and then for a few more poll periods, so a deployment the fetch
// queued has started by the time the caller reads the deploy log.
func (r *gitRig) settled(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return !r.m.Fetcher.IsFetching() && r.d.Idle() }, "comin is quiescent")
	time.Sleep(4 * r.m.pollPeriod)
}

func (r *gitRig) excludedOnDisk(t *testing.T) []leasestate.Head {
	t.Helper()
	onDisk, err := leasestate.Load(r.lsPath)
	require.NoError(t, err)
	return onDisk.ExcludedHeads()
}

// endedOverrideOnRemote leaves a released testing head t1 on testing-r1,
// the machine back on m1 and comin stopped with its
// lease-state.json moved aside. It returns the two commit hashes.
func endedOverrideOnRemote(t *testing.T, dir string, remote *gitRemote) (m1, t1 string) {
	t.Helper()
	m1 = remote.commit("", "m1")
	remote.setBranch("main", m1)
	r1 := newGitRig(t, dir, remote)
	r1.start(t)
	r1.fetch(t, "m1")
	r1.waitDeploys(t, "m1/switch")
	t1 = remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r1.fetch(t, "t1")
	r1.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r1.leaseExists(), "the hook took a git lease")
	require.NoError(t, os.Remove(r1.lease))
	r1.waitDrift(t, "leaseless")
	require.NoError(t, r1.m.SwitchDeploymentLatest())
	r1.waitDeploys(t, "m1/switch", "t1/test", "m1/switch")
	r1.waitReleasedOnDisk(t)
	r1.stop()
	require.NoError(t, os.Remove(r1.lsPath))
	return m1, t1
}

// The ended head is still on the remote and lease-state.json is
// gone: comin neither selects nor deploys it, so the hook takes no lease,
// and still not after a further restart, by which time the file exists
// again (a lease-state write between the starts: a memory-only exclusion
// is recomputed from an absent file and would not hold). Mutants: no
// exclusion for an absent file; the exclusion kept in memory only.
func TestAbsentLeaseStateExcludesEndedHeadAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1, t1 := endedOverrideOnRemote(t, dir, remote)
	onM1 := "/nix/store/" + m1
	wantHeads := []leasestate.Head{{Remote: "origin", Branch: "testing-r1", Commit: t1}}

	r2 := newGitRigRunning(t, dir, remote, onM1)
	require.NoError(t, r2.lsErr)
	assert.Equal(t, wantHeads, r2.excludedOnDisk(t), "the exclusion is persisted at start")
	r2.start(t)
	assert.Equal(t, "m1", r2.fetchSelected(t), "the ended head is not selected")
	r2.settled(t)
	assert.Empty(t, r2.deployLog())
	assert.False(t, r2.leaseExists(), "no hook call, so no lease")
	// A lease-state write, so the file exists at the next start.
	since := time.Now().UTC()
	require.NoError(t, r2.ls.SetDriftSince(&since))
	r2.stop()

	r3 := newGitRigRunning(t, dir, remote, onM1)
	require.NoError(t, r3.lsErr)
	assert.False(t, r3.ls.AbsentAtLoad(), "the file exists at the second start")
	r3.start(t)
	assert.Equal(t, "m1", r3.fetchSelected(t), "the ended head is not selected after a further restart")
	r3.settled(t)
	assert.Empty(t, r3.deployLog())
	assert.False(t, r3.leaseExists())
}

// A first start of the fork over a be6025e-shaped store {m1 switch, t1
// test} (no lease-state ever) whose PC runs t1: the exclusion exempts the
// running head, so the first fetch deploys nothing, and main moving to m2
// deploys m2, both as be6025e. Mutants: the absence recorded as
// MarksLostAt (m2 is refused, drift is leaseless); no exemption for the
// running head (the first fetch redeploys m1 over t1).
func TestAbsentLeaseStateFirstForkStartKeepsTheRunningTestAndMainDeploys(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r1 := newGitRig(t, dir, remote)
	r1.start(t)
	r1.fetch(t, "m1")
	r1.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r1.fetch(t, "t1")
	r1.waitDeploys(t, "m1/switch", "t1/test")
	r1.stop()
	// be6025e wrote neither the lease-state nor the lease.
	require.NoError(t, os.Remove(r1.lsPath))
	require.NoError(t, os.Remove(r1.lease))

	r2 := newGitRigRunning(t, dir, remote, "/nix/store/"+t1)
	require.NoError(t, r2.lsErr)
	assert.Empty(t, r2.excludedOnDisk(t), "the running head is not excluded")
	r2.start(t)
	assert.Equal(t, "t1", r2.fetchSelected(t), "the running test stays the selection")
	r2.settled(t)
	assert.Empty(t, r2.deployLog(), "nothing deploys over the developer's running test")

	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	assert.Equal(t, "m2", r2.fetchSelected(t))
	r2.waitDeploys(t, "m2/switch")
	r2.waitDrift(t, "none")
	assert.False(t, r2.leaseExists())
}

// After such a start a new testing head, a commit no stored row names,
// still deploys. Mutant: the head excluded by branch instead of by
// (remote, branch, commit).
func TestAbsentLeaseStateNewTestingHeadStillDeploys(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1, t1 := endedOverrideOnRemote(t, dir, remote)

	r2 := newGitRigRunning(t, dir, remote, "/nix/store/"+m1)
	r2.start(t)
	assert.Equal(t, "m1", r2.fetchSelected(t))
	r2.settled(t)
	require.Empty(t, r2.deployLog())

	t2 := remote.commit(m1, "t2")
	require.NotEqual(t, t1, t2)
	remote.setBranch("testing-r1", t2)
	assert.Equal(t, "t2", r2.fetchSelected(t))
	r2.waitDeploys(t, "t2/test")
}

// An absent file over a store without testing rows starts empty and
// writes nothing. Mutant: the exclusion is recorded (the file written)
// for an empty store.
func TestAbsentLeaseStateWithoutTestingRowsWritesNothing(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r1 := newGitRig(t, dir, remote)
	r1.start(t)
	r1.fetch(t, "m1")
	r1.waitDeploys(t, "m1/switch")
	r1.stop()
	// Deploying main wrote no lease-state.
	_, err := os.Stat(r1.lsPath)
	require.True(t, os.IsNotExist(err))

	r2 := newGitRigRunning(t, dir, remote, "/nix/store/"+m1)
	require.NoError(t, r2.lsErr)
	assert.True(t, r2.ls.AbsentAtLoad())
	assert.Empty(t, r2.ls.ExcludedHeads())
	_, err = os.Stat(r2.lsPath)
	assert.True(t, os.IsNotExist(err), "nothing is written for an empty store")
	r2.start(t)
	assert.Equal(t, "m1", r2.fetchSelected(t))
	r2.settled(t)
	assert.Empty(t, r2.deployLog())
}
