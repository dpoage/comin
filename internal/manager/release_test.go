package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/nlewo/comin/internal/lease"
	"github.com/nlewo/comin/internal/leasestate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// C3 (release without return) and the deploy gate's ownership rule,
// through the real repository and fetcher. The testing branch is left on
// the remote, as it is after any reboot: once the override is released the
// repository no longer selects its head, so the next fetch selects the
// main head again and a newer tier commit is selected, built and offered
// like on a robot. Every fixture runs in-process and across a comin
// restart.

func (g *gitRemote) head(branch string) string {
	ref, err := g.repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(g.t, err)
	return ref.Hash().String()
}

// startGitOverride deploys m1, then n testing heads t1..tn, each built on
// the previous one, through the fetcher. The hook takes a git lease after
// t1.
func startGitOverride(t *testing.T, dir string, n int) *gitRig {
	t.Helper()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, dir, remote)
	r.start(t)
	r.fetch(t, "m1")
	want := []string{"m1/switch"}
	r.waitDeploys(t, want...)
	tip := m1
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("t%d", i)
		tip = remote.commit(tip, name)
		remote.setBranch("testing-r1", tip)
		r.fetch(t, name)
		want = append(want, name+"/test")
		r.waitDeploys(t, want...)
	}
	require.True(t, r.leaseExists(), "the hook took a git lease")
	return r
}

// endLease removes the lease file. With restart, comin is stopped first
// and a new comin process is started on the same state, running what the
// machine ran (a reboot sets the current system before calling it).
func (r *gitRig) endLease(t *testing.T, restart bool) *gitRig {
	t.Helper()
	if !restart {
		require.NoError(t, os.Remove(r.lease))
		return r
	}
	r.stop()
	require.NoError(t, os.Remove(r.lease))
	return r.restart(t)
}

func (r *gitRig) restart(t *testing.T) *gitRig {
	t.Helper()
	r.stop()
	next := newGitRig(t, filepath.Dir(r.lsPath), r.remote)
	next.exec.set(r.exec.get())
	next.start(t)
	return next
}

// waitReleasedOnDisk waits until every testing deployment in the store is
// recorded released in comin's lease state file.
func (r *gitRig) waitReleasedOnDisk(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool {
		onDisk, err := leasestate.Load(r.lsPath)
		if err != nil {
			return false
		}
		n := 0
		for _, d := range r.s.DeploymentList() {
			if !isTestingDeployment(d) {
				continue
			}
			n++
			if !onDisk.IsReleased(d.Uuid) {
				return false
			}
		}
		return n > 0
	}, "every testing deployment is released in the lease state file")
}

func (r *gitRig) deferredCommit() string {
	r.m.deferredMu.Lock()
	defer r.m.deferredMu.Unlock()
	if r.m.deferred == nil {
		return ""
	}
	return r.m.deferred.SelectedCommitId
}

// waitMainDecided waits until the deploy gate has decided on the fetched
// main commit hash: deferred, or deployed.
func (r *gitRig) waitMainDecided(t *testing.T, hash string) {
	t.Helper()
	name := r.remote.names[hash]
	waitFor(t, func() bool {
		return r.deferredCommit() == hash || slices.Contains(r.named(), name+"/switch")
	}, "the gate never decided on %s", name)
}

// assertLeftAlone checks that comin deploys nothing over the running
// system: S4 reports state, and a newer tier commit is fetched, built and
// deferred, and stays deferred over several polls. The deploy log stays
// log.
func (r *gitRig) assertLeftAlone(t *testing.T, state string, log ...string) {
	t.Helper()
	current := r.exec.get()
	r.waitDrift(t, state)
	name := fmt.Sprintf("m-next-%d", len(r.remote.names))
	next := r.remote.commit(r.remote.head("main"), name)
	r.remote.setBranch("main", next)
	r.fetch(t, name)
	r.waitMainDecided(t, next)
	for range 3 {
		r.m.poll(time.Now().UTC())
	}
	r.waitDeploys(t, log...)
	assert.Equal(t, next, r.deferredCommit(), "the tier commit stays deferred")
	assert.Equal(t, current, r.exec.get(), "the running system is left as it is")
	r.waitDrift(t, state)
}

func variants(t *testing.T, run func(t *testing.T, restart bool)) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) { run(t, restart) })
	}
}

// The operator ends the git lease (the file is removed, no switch-latest):
// the testing deployments are released and the system is left on the
// override, though the next fetch selects m1 again. Mutants: the gate
// admits fetched main over a system comin does not own; released marks in
// memory only.
func TestC3OperatorEndedLeaseDeploysNothing(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r := startGitOverride(t, t.TempDir(), n)
				log := r.named()
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m1")
				r.assertLeftAlone(t, "leaseless", log...)
				assert.Equal(t, fmt.Sprintf("t%d", n), r.current())
			})
		})
	}
}

// Same, then the operator's switch-latest: exactly one deployment, to m1,
// then S4 none, and a newer tier commit deploys normally. Mutant: the gate
// deploys a main commit that is already running.
func TestC3OperatorEndedLeaseThenSwitchLatest(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r := startGitOverride(t, t.TempDir(), n)
				log := r.named()
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m1")
				r.waitDrift(t, "leaseless")

				require.NoError(t, r.m.SwitchDeploymentLatest())
				log = append(log, "m1/switch")
				r.waitDeploys(t, log...)
				r.waitDrift(t, "none")

				m1 := r.remote.head("main")
				m2 := r.remote.commit(m1, "m2")
				r.remote.setBranch("main", m2)
				r.fetch(t, "m2")
				r.waitDeploys(t, append(log, "m2/switch")...)
				r.waitDrift(t, "none")
			})
		})
	}
}

// The lease ends by reboot: the machine runs m1 again, so comin owns it,
// deploys nothing for the override (m1, selected again, is already
// running), and a newer tier commit deploys. Mutant: the gate deploys a
// main commit that is already running.
func TestC3RebootEndedLeaseDeploysNothingThenTierResumes(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r := startGitOverride(t, t.TempDir(), n)
				log := r.named()
				r.exec.set("/nix/store/" + r.remote.head("main"))
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m1")
				r.waitDrift(t, "none")
				r.waitDeploys(t, log...)

				m2 := r.remote.commit(r.remote.head("main"), "m2")
				r.remote.setBranch("main", m2)
				r.fetch(t, "m2")
				r.waitDeploys(t, append(log, "m2/switch")...)
				r.waitDrift(t, "none")
			})
		})
	}
}

// Ruling 1: a break-glass activated while the git lease was held is not
// comin's; when the lease ends it is left as it is, whatever the tier
// does.
func TestC3BreakGlassDuringGitLeaseDeploysNothing(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r := startGitOverride(t, t.TempDir(), n)
				log := r.named()
				r.exec.set("/nix/store/break-glass")
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m1")
				r.assertLeftAlone(t, "leaseless", log...)
				assert.Equal(t, "/nix/store/break-glass", r.exec.get())
			})
		})
	}
}

// A git lease ends by reboot, then the system is changed out of band:
// before comin starts again (restart), or while it runs (in-process).
func TestC3RebootEndedLeaseThenOutOfBandChangeDeploysNothing(t *testing.T) {
	variants(t, func(t *testing.T, restart bool) {
		r := startGitOverride(t, t.TempDir(), 1)
		log := r.named()
		if restart {
			r.stop()
			require.NoError(t, os.Remove(r.lease))
			r.exec.set("/nix/store/out-of-band")
			r = r.restart(t)
			log = []string{}
		} else {
			r.exec.set("/nix/store/" + r.remote.head("main"))
			r = r.endLease(t, false)
			r.waitReleasedOnDisk(t)
			r.waitDrift(t, "none")
			r.exec.set("/nix/store/out-of-band")
		}
		r.waitReleasedOnDisk(t)
		r.fetch(t, "m1")
		r.assertLeftAlone(t, "leaseless", log...)
	})
}

// A closure lease is held and the closure activated out of band; the tier
// commit fetched meanwhile is deferred by C1. When the lease ends (the
// lease tool, not comin, decides whether to return a closure) the deferred
// commit is not deployed over the closure. Mutant: the deferred
// generation is offered without the gate.
func TestC1DeferredTierCommitNotFlushedOverClosure(t *testing.T) {
	variants(t, func(t *testing.T, restart bool) {
		remote := newGitRemote(t)
		m1 := remote.commit("", "m1")
		remote.setBranch("main", m1)
		r := newGitRig(t, t.TempDir(), remote)
		r.start(t)
		r.fetch(t, "m1")
		r.waitDeploys(t, "m1/switch")

		r.writeLease(t, "closure")
		r.exec.set("/nix/store/closure")
		m2 := remote.commit(m1, "m2")
		remote.setBranch("main", m2)
		r.fetch(t, "m2")
		r.waitMainDecided(t, m2)
		r.waitDrift(t, "held")

		log := []string{"m1/switch"}
		r = r.endLease(t, restart)
		if restart {
			log = []string{}
			r.fetch(t, "m2")
			r.waitMainDecided(t, m2)
		}
		for range 3 {
			r.m.poll(time.Now().UTC())
		}
		r.waitDeploys(t, log...)
		assert.Equal(t, m2, r.deferredCommit())
		r.assertLeftAlone(t, "leaseless", log...)
		assert.Equal(t, "/nix/store/closure", r.exec.get())
	})
}

// The tier moves to m2 while the git lease is held: m2 is fetched, built
// and deferred. The operator ends the lease with the override running:
// nothing deploys. switch-latest deploys the S4 expected generation, m1
// (m2 never deployed), after which the machine is comin's again and the
// deferred m2 follows.
func TestC3TierMovedDuringLeaseThenSwitchLatest(t *testing.T) {
	variants(t, func(t *testing.T, restart bool) {
		r := startGitOverride(t, t.TempDir(), 1)
		m1 := r.remote.head("main")
		m2 := r.remote.commit(m1, "m2")
		r.remote.setBranch("main", m2)
		r.fetch(t, "m2")
		r.waitMainDecided(t, m2)
		log := []string{"m1/switch", "t1/test"}
		r.waitDeploys(t, log...)

		r = r.endLease(t, restart)
		if restart {
			log = []string{}
			r.fetch(t, "m2")
			r.waitMainDecided(t, m2)
		}
		r.waitReleasedOnDisk(t)
		for range 3 {
			r.m.poll(time.Now().UTC())
		}
		r.waitDeploys(t, log...)
		r.waitDrift(t, "leaseless")
		assert.Equal(t, "t1", r.current())

		require.NoError(t, r.m.SwitchDeploymentLatest())
		r.waitDeploys(t, append(log, "m1/switch", "m2/switch")...)
		r.waitDrift(t, "none")
		assert.Equal(t, "m2", r.current())
	})
}

// overrideLeaseFile null: be6025e's decisions. A testing head deploys
// with test, a tier commit deploys over a system comin did not deploy, a
// testing head built on it deploys, a lease file at the unconfigured path
// changes nothing, and after a restart the re-emitted current head is
// skipped as already deployed.
func TestNullLeaseFileDeploysAsBe6025e(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, dir, remote)
	r.m.leaseReader = lease.NewReader("")
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r.fetch(t, "t1")
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists(), "the hook wrote a lease file, at a path comin does not read")

	r.exec.set("/nix/store/break-glass")
	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	r.fetch(t, "m2")
	r.waitDeploys(t, "m1/switch", "t1/test", "m2/switch")
	t2 := remote.commit(m2, "t2")
	remote.setBranch("testing-r1", t2)
	r.fetch(t, "t2")
	r.waitDeploys(t, "m1/switch", "t1/test", "m2/switch", "t2/test")
	require.NoError(t, os.Remove(r.lease))
	r.stop()

	r2 := newGitRig(t, dir, remote)
	r2.m.leaseReader = lease.NewReader("")
	r2.exec.set("/nix/store/" + m1)
	r2.start(t)
	r2.fetch(t, "t2")
	m3 := remote.commit(m2, "m3")
	remote.setBranch("main", m3)
	r2.fetch(t, "m3")
	r2.waitDeploys(t, "m3/switch")
	assert.False(t, r2.ls.PendingSwitchLatest())
	for _, d := range r2.s.DeploymentList() {
		assert.False(t, r2.ls.IsReleased(d.Uuid))
	}
}

// ---------------------------------------------------------------------
// The tier after an override: a released testing head never shadows it
// ---------------------------------------------------------------------

func (g *gitRemote) hash(name string) string {
	for h, n := range g.names {
		if n == name {
			return h
		}
	}
	g.t.Fatalf("no commit named %s", name)
	return ""
}

// tierShadowingOverride starts an override whose testing head descends
// from the newer tier commit m2, so the repository prefers that head to m2
// for as long as it is the branch head and not excluded. It returns the
// rig and its deploy log. Shapes:
//   - rebuilt: m1, then t1 under the git lease; main moves to m2, fetched
//     alone and deferred; the testing branch is rebuilt on m2 as t2.
//   - one fetch: m1, then t1 under the git lease; m2 and t2 built on it
//     arrive in one fetch, so m2 never reaches the deploy gate.
//   - lagging: the robot runs m1 with no lease; m2 and t1 built on it
//     arrive in one fetch; t1 deploys and the hook takes the lease.
func tierShadowingOverride(t *testing.T, dir, shape string) (*gitRig, []string) {
	t.Helper()
	if shape == "lagging" {
		remote := newGitRemote(t)
		m1 := remote.commit("", "m1")
		remote.setBranch("main", m1)
		r := newGitRig(t, dir, remote)
		r.start(t)
		r.fetch(t, "m1")
		r.waitDeploys(t, "m1/switch")
		m2 := remote.commit(m1, "m2")
		remote.setBranch("main", m2)
		remote.setBranch("testing-r1", remote.commit(m2, "t1"))
		r.fetch(t, "t1")
		log := []string{"m1/switch", "t1/test"}
		r.waitDeploys(t, log...)
		require.True(t, r.leaseExists(), "the hook took a git lease")
		return r, log
	}
	r := startGitOverride(t, dir, 1)
	m2 := r.remote.commit(r.remote.head("main"), "m2")
	r.remote.setBranch("main", m2)
	if shape == "rebuilt" {
		r.fetch(t, "m2")
		r.waitMainDecided(t, m2)
	}
	r.remote.setBranch("testing-r1", r.remote.commit(m2, "t2"))
	r.fetch(t, "t2")
	log := []string{"m1/switch", "t1/test", "t2/test"}
	r.waitDeploys(t, log...)
	return r, log
}

// B-1 and A-d (rebuilt), B-3 (rebuilt, the remote testing branch deleted;
// comin's clone keeps it), and the other shapes: the lease ends by reboot,
// back on m1, so comin owns the system. After C3's release, one fetch
// selects m2 and it deploys, exactly once. Mutants: the repository's
// testing selection does not exclude released heads; the gate deploys a
// main commit that is already running (in-process, the deferred m2 and
// the fetched m2 both deploy).
func TestTierDeploysAfterRebootEndedOverride(t *testing.T) {
	for _, c := range []struct {
		shape        string
		deleteBranch bool
	}{{"rebuilt", false}, {"rebuilt", true}, {"one fetch", false}, {"lagging", false}} {
		t.Run(fmt.Sprintf("%s/branch deleted=%v", c.shape, c.deleteBranch), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r, log := tierShadowingOverride(t, t.TempDir(), c.shape)
				if c.deleteBranch {
					r.remote.deleteBranch("testing-r1")
				}
				r.exec.set("/nix/store/" + r.remote.hash("m1"))
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m2")
				r.waitDeploys(t, append(log, "m2/switch")...)
				r.waitDrift(t, "none")
				assert.Equal(t, "m2", r.current())
			})
		})
	}
}

// B-2 and A-a (rebuilt), A-b (one fetch), A-c (lagging): the operator
// ends the lease with the override running. After C3's release one fetch
// selects m2, which is deferred: comin does not own the system. The
// operator's switch-latest deploys m1, the S4 expected, then comin owns the
// system and m2 follows. comin restarts never, while the lease is held
// (the deferred m2 is lost), or when the lease ends. Mutant: the
// repository's testing selection does not exclude released heads.
func TestTierDeploysAfterOperatorEndedOverrideAndSwitchLatest(t *testing.T) {
	for _, shape := range []string{"rebuilt", "one fetch", "lagging"} {
		for _, restart := range []string{"never", "during the lease", "at the end"} {
			t.Run(shape+"/restart "+restart, func(t *testing.T) {
				r, log := tierShadowingOverride(t, t.TempDir(), shape)
				head, _, _ := strings.Cut(log[len(log)-1], "/")
				if restart == "during the lease" {
					r = r.restart(t)
					log = []string{}
					r.fetch(t, head)
					r.waitDeploys(t)
				}
				r = r.endLease(t, restart == "at the end")
				if restart == "at the end" {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				m2 := r.remote.hash("m2")
				r.fetch(t, "m2")
				r.waitMainDecided(t, m2)
				for range 3 {
					r.m.poll(time.Now().UTC())
				}
				r.waitDeploys(t, log...)
				r.waitDrift(t, "leaseless")
				assert.Equal(t, head, r.current())

				require.NoError(t, r.m.SwitchDeploymentLatest())
				r.waitDeploys(t, append(log, "m1/switch", "m2/switch")...)
				r.waitDrift(t, "none")
				assert.Equal(t, "m2", r.current())
			})
		}
	}
}

// ---------------------------------------------------------------------
// A rollback to an earlier build of comin's
// ---------------------------------------------------------------------

// deployTwoMainCommits deploys m1, then m2, through the fetcher.
func deployTwoMainCommits(t *testing.T, dir string) *gitRig {
	t.Helper()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, dir, remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	remote.setBranch("main", remote.commit(m1, "m2"))
	r.fetch(t, "m2")
	r.waitDeploys(t, "m1/switch", "m2/switch")
	return r
}

// Another tool (the platform's update watchdog) condemns m2 and activates
// m1 again. m1 is comin's own earlier build: comin never activates m2
// again on its own, in-process or when a restart re-emits it, S4 reports
// leaseless drift, and a newer main commit m3 deploys. A break-glass
// system in m1's place is not comin's: m3 is deferred and the break-glass
// left running. Mutant: comin owns only the S4 expected system and its
// latest deployment's.
func TestRollbackToOwnBuildThenNewerMainDeploys(t *testing.T) {
	variants(t, func(t *testing.T, restart bool) {
		t.Run("rolled back to m1", func(t *testing.T) {
			r := deployTwoMainCommits(t, t.TempDir())
			r.exec.set("/nix/store/" + r.remote.hash("m1"))
			log := []string{"m1/switch", "m2/switch"}
			if restart {
				r = r.restart(t)
				log = []string{}
				r.fetch(t, "m2")
			}
			for range 3 {
				r.m.poll(time.Now().UTC())
			}
			r.waitDeploys(t, log...)
			r.waitDrift(t, "leaseless")
			assert.Equal(t, "m1", r.current())

			r.remote.setBranch("main", r.remote.commit(r.remote.hash("m2"), "m3"))
			r.fetch(t, "m3")
			r.waitDeploys(t, append(log, "m3/switch")...)
			r.waitDrift(t, "none")
		})
		t.Run("break-glass", func(t *testing.T) {
			r := deployTwoMainCommits(t, t.TempDir())
			r.exec.set("/nix/store/break-glass")
			log := []string{"m1/switch", "m2/switch"}
			if restart {
				r = r.restart(t)
				log = []string{}
				r.fetch(t, "m2")
			}
			r.assertLeftAlone(t, "leaseless", log...)
			assert.Equal(t, "/nix/store/break-glass", r.exec.get())
		})
	})
}

// comin is rolled back from m2 to m1, then n testing heads built on m2
// deploy over m1 (comin's own) and the hook takes a git lease. The
// override ends with the machine back on m1 (a reboot to the profile the
// rollback left, or the watchdog rolling the testing system back): once
// the override is released the repository selects m2 again and emits it.
// The deployer's same-commit dedup compares it with the deployment before
// the latest one in-process, and with the latest one after a restart, so
// it only stops m2 in-process after a single testing deployment. comin
// still never re-activates m2 on its own; m3 deploys. Mutant: no
// rolled-back rule in the gate.
func TestRolledBackBuildNotReactivatedAfterOverride(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d testing deploys", n), func(t *testing.T) {
			variants(t, func(t *testing.T, restart bool) {
				r := deployTwoMainCommits(t, t.TempDir())
				m1, m2 := r.remote.hash("m1"), r.remote.hash("m2")
				r.exec.set("/nix/store/" + m1)
				log := []string{"m1/switch", "m2/switch"}
				tip := m2
				for i := 1; i <= n; i++ {
					name := fmt.Sprintf("t%d", i)
					tip = r.remote.commit(tip, name)
					r.remote.setBranch("testing-r1", tip)
					r.fetch(t, name)
					log = append(log, name+"/test")
					r.waitDeploys(t, log...)
				}
				require.True(t, r.leaseExists(), "the hook took a git lease")

				r.exec.set("/nix/store/" + m1)
				r = r.endLease(t, restart)
				if restart {
					log = []string{}
				}
				r.waitReleasedOnDisk(t)
				r.fetch(t, "m2")
				for range 3 {
					r.m.poll(time.Now().UTC())
				}
				r.waitDeploys(t, log...)
				r.waitDrift(t, "leaseless")
				assert.Equal(t, "m1", r.current())

				r.remote.setBranch("main", r.remote.commit(m2, "m3"))
				r.fetch(t, "m3")
				r.waitDeploys(t, append(log, "m3/switch")...)
				r.waitDrift(t, "none")
			})
		})
	}
}
