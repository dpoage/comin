package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *rig) testingDeploymentUuid(t *testing.T, commit string) string {
	t.Helper()
	for _, d := range r.s.DeploymentList() {
		if isTestingDeployment(d) && d.Generation.SelectedCommitId == commit {
			return d.Uuid
		}
	}
	t.Fatalf("no testing deployment of %s in the store", commit)
	return ""
}

// overrideEnded removes the git lease with the stage holding a testing
// generation, lets C3 tick, then requests switch-latest. C3 must record
// nothing until the stage is empty.
func (r *rig) overrideEnded(t *testing.T, t1 string) {
	t.Helper()
	require.NoError(t, os.Remove(r.lease))
	require.Never(t, func() bool {
		pollNow(r)
		return r.ls.IsReleased(t1)
	}, 300*time.Millisecond, 10*time.Millisecond, "C3 released the override while the stage held a testing generation")
	require.NoError(t, r.m.SwitchDeploymentLatest())
	waitFor(t, func() bool {
		return slices.Equal(r.deployLog(), []string{"m1/switch", "t1/test", "m1/switch"}) && r.d.Idle()
	}, "switch-latest never deployed m1, got %v", r.deployLog())
}

func (r *rig) assertT2NeverDeploys(t *testing.T, h *hookCtl, t1 string) {
	t.Helper()
	waitFor(t, r.stageIdle, "the stage is idle")
	assert.Equal(t, []string{"0"}, h.exits("gen-t2"))
	waitFor(t, func() bool { return r.ls.IsReleased(t1) }, "C3 never recorded t1 released once the stage was empty")
	require.Never(t, func() bool { return len(r.deployLog()) > 3 || r.leaseExists() }, 300*time.Millisecond, 10*time.Millisecond, "t2 deployed or the lease was taken again")
	assert.Equal(t, []string{"m1/switch", "t1/test", "m1/switch"}, r.deployLog())
}

func TestC3StageHoldsReleaseWhileTestingGenerationRuns(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t1", "m1")))
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists(), "the post-deployment command took a git lease")
	t1 := r.testingDeploymentUuid(t, "t1")

	h.set(t, "block-gen-t2")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t2", "m1")))
	h.waitStarts(t, "gen-t2", 1)
	require.Equal(t, "gen-t2", r.m.preDeploy.Status().Running.Generation.Uuid)

	r.overrideEnded(t, t1)
	h.clear("block-gen-t2")
	r.assertT2NeverDeploys(t, h, t1)
}

func TestC3StageHoldsReleaseWhileTestingGenerationPending(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "block-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m2", "/nix/store/m2")))
	h.waitStarts(t, "gen-m2", 1)
	require.Equal(t, "submitted:test", r.fetchDeploy(testingGeneration("t1", "m1")))
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists(), "the post-deployment command took a git lease")
	t1 := r.testingDeploymentUuid(t, "t1")

	h.set(t, "block-gen-t2")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t2", "m1")))
	waitFor(t, func() bool {
		st := r.m.preDeploy.Status()
		return st.Running != nil && st.Running.Generation.Uuid == "gen-m2" && st.Pending != nil && st.Pending.Generation.Uuid == "gen-t2"
	}, "t2 pending behind the running m2")

	r.overrideEnded(t, t1)
	h.clear("block-gen-m2")
	h.clear("block-gen-t2")
	r.assertT2NeverDeploys(t, h, t1)
	assert.Equal(t, []string{"0"}, h.exits("gen-m2"))
}

func TestPreDeployLeaseDriftIgnoresStage(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	hookDeployMain(t, r, "m2")
	r.exec.set("/nix/store/m1")
	r.waitDrift(t, "leaseless")

	h.set(t, "block-all")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m3", "/nix/store/m3")))
	h.waitStarts(t, "gen-m3", 1)
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m4", "/nix/store/m4")))
	waitFor(t, func() bool { return r.m.preDeploy.Status().Pending != nil }, "m4 pending")
	for range 10 {
		pollNow(r)
		require.Equal(t, "leaseless", r.drift().State, "the stage must be invisible to S4")
		time.Sleep(20 * time.Millisecond)
	}

	h.clear("block-all")
	r.waitDeploys(t, "m1/switch", "m2/switch", "m4/switch")
}

func TestPreDeployLeaseSuspendedManagerRunsHookAndDeploysOnResume(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	dir := t.TempDir()
	r := newGitRig(t, dir, remote)
	h := installHook(t, r.m, dir)
	h.set(t, "block-all")
	r.start(t)
	r.fetch(t, "m1")
	h.waitStarts(t, m1, 1)

	// No deployment has finished yet: Manager.Run reads isSuspended after each one.
	require.NoError(t, r.m.Suspend())
	require.Equal(t, "submitted:switch", hookTail(r.rig, mainGeneration("m2", "/nix/store/m2")))
	h.clear("block-all")
	h.waitExits(t, "gen-m2", "0")
	waitFor(t, r.stageIdle, "the stage is idle")
	require.Never(t, func() bool { return len(r.deployLog()) > 0 }, 300*time.Millisecond, 10*time.Millisecond, "m2 deployed while the manager was suspended")
	assert.Equal(t, 1, h.starts("gen-m2"))

	// The gate orders Resume's write of isSuspended before Run's read.
	gate := r.holdDeploys()
	require.NoError(t, r.m.Resume(t.Context()))
	r.releaseDeploys(gate)
	r.rig.waitDeploys(t, "m2/switch")
	assert.Equal(t, 1, h.starts("gen-m2"), "the command is not run again on resume")
	assert.Equal(t, 1, h.starts(m1), "m1 was superseded in the stage")
}

func TestPreDeployLeaseRestartMidHookRunsHookAgain(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	dir := t.TempDir()
	r := newGitRig(t, dir, remote)
	h := installHook(t, r.m, dir)
	h.set(t, "block-all")
	r.start(t)
	r.fetch(t, "m1")
	h.waitStarts(t, m1, 1)

	// The rig's stop cancels the stage context, which kills the command. In
	// production comin's context is never cancelled; systemd's control-group
	// kill ends the command when it stops or restarts the comin unit.
	r.stop()
	assert.Empty(t, r.deployLog())
	assert.Empty(t, h.exits(m1), "the rig's stop killed the command")
	h.clear("block-all")

	next := newGitRig(t, dir, remote)
	installHook(t, next.m, dir)
	next.start(t)
	next.fetch(t, "m1")
	next.waitDeploys(t, "m1/switch")
	assert.Equal(t, 2, h.starts(m1), "the restarted comin ran the command again")
	assert.Equal(t, []string{"0"}, h.exits(m1))
}

func TestPreDeployUnitKillModeKeepsControlGroup(t *testing.T) {
	if _, err := exec.LookPath("nix"); err != nil {
		t.Skip("nix not found on PATH; this test requires the nix devshell/gate environment")
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	require.NoError(t, err)
	expr := `
let
  flake = builtins.getFlake "` + root + `";
  eval = flake.inputs.nixpkgs.lib.nixosSystem {
    system = builtins.currentSystem;
    modules = [
      flake.nixosModules.comin
      {
        boot.loader.grub.enable = false;
        fileSystems."/" = { device = "none"; fsType = "tmpfs"; };
        system.stateVersion = "24.11";
        services.comin = {
          enable = true;
          remotes = [ { name = "origin"; url = "https://example.org/r.git"; } ];
          preDeploymentCommand = "/nix/store/x-hook";
        };
      }
    ];
  };
in eval.config.systemd.units."comin.service".text
`
	out, err := exec.Command("nix", "eval",
		"--extra-experimental-features", "nix-command flakes",
		"--impure", "--raw", "--expr", expr,
	).CombinedOutput()
	require.NoError(t, err, "nix eval failed: %s", out)
	unit := string(out)
	require.Contains(t, unit, "[Service]")
	require.Regexp(t, regexp.MustCompile(`(?m)^ExecStart=.*comin run --config`), unit)
	for _, m := range regexp.MustCompile(`(?m)^KillMode=(.*)$`).FindAllStringSubmatch(unit, -1) {
		assert.Equal(t, "control-group", m[1], "a restart must kill the pre-deployment command with comin")
	}
}
