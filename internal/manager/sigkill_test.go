package manager

import (
	"os"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deployment store row is written only when the deployer settles a
// deployment, so a comin killed after switch-to-configuration repointed
// /run/current-system but before that leaves the machine on a system no
// stored deployment names. The restarted comin must deploy the offered
// generation of that system (the activation is idempotent, and the
// post-deployment command takes the lease), not refuse it as foreign.

// sigkilled replays the kill: m1 is deployed and stored, then g is
// submitted with its deployment held after the activation repointed the
// current system, and this comin process ends. It returns the restarted
// rig (on the same dir, its executor reporting the current system the
// killed comin left), with the generation to offer on it. The killed
// comin's deployer is released when the test ends, so its cleanup can
// wait for it.
func sigkilled(t *testing.T, g *protobuf.Generation, wantSubmit string) *rig {
	t.Helper()
	dir := t.TempDir()
	r1 := newRig(t, dir)
	r1.start(t)
	require.Equal(t, "submitted:switch", r1.fetchDeploy(mainGeneration("m1", "/nix/store/m1")))
	r1.waitDeploys(t, "m1/switch")

	gate := r1.holdDeploys()
	require.Equal(t, wantSubmit, r1.fetchDeploy(g))
	waitFor(t, func() bool { return r1.exec.get() == g.OutPath }, "the activation repointed the current system")
	r1.stop()

	r2 := newRig(t, dir)
	r2.exec.set(g.OutPath)
	for _, d := range r2.s.DeploymentList() {
		require.NotEqual(t, g.OutPath, d.Generation.GetOutPath(), "the killed deployment was never stored")
	}
	// Registered after both rigs, so it runs before their cleanups wait
	// for the killed comin's deployer.
	t.Cleanup(func() { r1.releaseDeploys(gate) })
	r2.start(t)
	return r2
}

// C2.P1, testing: the tip of the testing branch whose system the killed
// comin had activated deploys with operation test after the restart, and
// the hook takes the lease. Mutant: no running-system clause in
// ownsRunningSystem.
func TestSigkillMidTestingSwitchDeploysTipAfterRestart(t *testing.T) {
	tip := testingGeneration("t1", "m1")
	r := sigkilled(t, tip, "submitted:test")
	assert.False(t, r.leaseExists(), "the killed comin never ran the hook")

	assert.Equal(t, "submitted:test", r.fetchDeploy(tip))
	r.waitDeploys(t, "t1/test")
	assert.True(t, r.leaseExists(), "the hook took the lease")
}

// C2.P1, main: the same for a main generation.
func TestSigkillMidMainSwitchDeploysAfterRestart(t *testing.T) {
	g := mainGeneration("m2", "/nix/store/m2")
	r := sigkilled(t, g, "submitted:switch")

	assert.Equal(t, "submitted:switch", r.fetchDeploy(g))
	r.waitDeploys(t, "m2/switch")
}

// C2.P1, deferred: a main generation the gate deferred while the running
// system was foreign is offered again by the poll once the running system
// is the one it builds. Mutant: offerDeferred asks ownsRunningSystem
// without the generation.
func TestDeferredMainOfferedWhenItsSystemRuns(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m1", "/nix/store/m1")))
	r.waitDeploys(t, "m1/switch")

	r.exec.set("/nix/store/break-glass")
	g := mainGeneration("m2", "/nix/store/m2")
	require.Equal(t, "skipped:lease-decision", r.fetchDeploy(g))
	time.Sleep(200 * time.Millisecond)
	r.waitDeploys(t, "m1/switch")

	r.exec.set("/nix/store/m2")
	r.waitDeploys(t, "m1/switch", "m2/switch")
}

// The lease checks run before ownership: with a lease the running system
// being the offered one admits nothing.
func TestSigkillRunningSystemDoesNotOverrideLeaseChecks(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m1", "/nix/store/m1")))
	r.waitDeploys(t, "m1/switch")

	r.writeLease(t, "closure")
	r.exec.set("/nix/store/t1")
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(testingGeneration("t1", "m1")))
	r.exec.set("/nix/store/m2")
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
	r.waitDeploys(t, "m1/switch")
}

// C2.P2: a running system that is not the offered generation's stays
// foreign: nothing deploys over a break-glass.
func TestOutOfBandRunningSystemStaysRefused(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m1", "/nix/store/m1")))
	r.waitDeploys(t, "m1/switch")

	r.exec.set("/nix/store/break-glass")
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(testingGeneration("t1", "m1")))
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/m2")))
	r.waitDeploys(t, "m1/switch")
}

// C2.P2: the system of an ended override stays foreign, however the
// generation that builds it is offered: a new commit with the identical
// out path (testing or main), and the released head itself. Mutant: no
// ended-override exclusion on the running-system clause.
func TestEndedOverrideSystemRefusedWhenReoffered(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.overrideOn(t, 1)
	x := r.d.Deployment()
	require.NoError(t, os.Remove(r.lease))
	waitFor(t, func() bool { return r.ls.IsReleased(x.Uuid) }, "C3 released the override")
	require.Equal(t, "/nix/store/t1", r.exec.get(), "the machine stays on the override's system")

	sameOutTesting := testingGeneration("t1b", "m1")
	sameOutTesting.OutPath = "/nix/store/t1"
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(sameOutTesting), "a new testing commit with the identical out path")
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(mainGeneration("m2", "/nix/store/t1")), "a main commit with the identical out path")
	assert.Equal(t, "skipped:lease-decision", r.fetchDeploy(rebuilt(testingGeneration("t1", "m1"))), "the released head")
	r.waitDeploys(t, "m1/switch", "t1/test")
}
