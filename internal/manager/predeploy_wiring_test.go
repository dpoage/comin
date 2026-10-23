package manager

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/lease"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookCtl drives the pre-deployment command of a manager under test. The
// script logs "start <uuid> <sha>" when it starts and "exit <uuid> <sha>
// <code>" when it ends; it waits while block-<uuid> or block-all exists and
// exits 1 once for failonce-<uuid> or always for fail-<uuid>.
type hookCtl struct {
	dir string
}

type hookRun struct {
	kind, uuid, sha, code string
}

const hookScript = `#!/bin/sh
d='@DIR@'
u="$COMIN_GENERATION"
echo "start $u $COMIN_GIT_SHA" >> "$d/runs"
while [ -e "$d/block-$u" ] || [ -e "$d/block-all" ]; do sleep 0.02; done
code=0
if [ -e "$d/failonce-$u" ]; then rm -f "$d/failonce-$u"; code=1; fi
if [ -e "$d/fail-$u" ]; then code=1; fi
echo "exit $u $COMIN_GIT_SHA $code" >> "$d/runs"
exit $code
`

// installHook sets the command of m; dir must be removed after m stops.
func installHook(t *testing.T, m *Manager, dir string) *hookCtl {
	t.Helper()
	h := &hookCtl{dir: filepath.Join(dir, "predeploy")}
	require.NoError(t, os.MkdirAll(h.dir, 0755))
	script := filepath.Join(h.dir, "hook.sh")
	require.NoError(t, os.WriteFile(script, []byte(strings.ReplaceAll(hookScript, "@DIR@", h.dir)), 0755))
	m.SetPreDeploymentCommand(script)
	return h
}

func (h *hookCtl) set(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, name), nil, 0644))
}

func (h *hookCtl) clear(name string) {
	_ = os.Remove(filepath.Join(h.dir, name))
}

func (h *hookCtl) runs() []hookRun {
	data, err := os.ReadFile(filepath.Join(h.dir, "runs"))
	if err != nil {
		return nil
	}
	var out []hookRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		run := hookRun{kind: f[0], uuid: f[1], sha: f[2]}
		if len(f) > 3 {
			run.code = f[3]
		}
		out = append(out, run)
	}
	return out
}

// starts counts the runs for who, a generation uuid or a commit.
func (h *hookCtl) starts(who string) int {
	n := 0
	for _, r := range h.runs() {
		if r.kind == "start" && (r.uuid == who || r.sha == who) {
			n++
		}
	}
	return n
}

// exits lists the exit codes of the finished runs for who.
func (h *hookCtl) exits(who string) []string {
	out := []string{}
	for _, r := range h.runs() {
		if r.kind == "exit" && (r.uuid == who || r.sha == who) {
			out = append(out, r.code)
		}
	}
	return out
}

func (h *hookCtl) waitStarts(t *testing.T, who string, n int) {
	t.Helper()
	waitFor(t, func() bool { return h.starts(who) >= n }, "the command never started %d times for %s", n, who)
}

func (h *hookCtl) waitExits(t *testing.T, who string, want ...string) {
	t.Helper()
	waitFor(t, func() bool { return slices.Equal(h.exits(who), want) }, "the command exits for %s are %v, want %v", who, h.exits(who), want)
}

// hookTail is the tail of FetchAndBuild for a built generation, with
// submitAdmitted in place of the deployer's Submit.
func hookTail(r *rig, g *protobuf.Generation) string {
	if r.d.IsAlreadyDeployed(g) {
		return "skipped:already-deployed"
	}
	op, ok := r.m.leaseDeployDecision(g)
	if !ok {
		return "skipped:lease-decision"
	}
	r.m.submitAdmitted(g, op)
	return "submitted:" + op
}

func hookDeployMain(t *testing.T, r *rig, commit string) {
	t.Helper()
	want := append(r.deployLog(), commit+"/switch")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration(commit, "/nix/store/"+commit)))
	r.waitDeploys(t, want...)
}

func newHookRig(t *testing.T) (*rig, *hookCtl) {
	t.Helper()
	dir := t.TempDir()
	r := newRig(t, dir)
	h := installHook(t, r.m, dir)
	r.start(t)
	return r, h
}

func (r *rig) stageIdle() bool { return !r.m.preDeployBusy() }

func pollNow(r *rig) { r.m.poll(time.Now().UTC()) }

func TestPreDeployNullCommandLeavesNoStage(t *testing.T) {
	r := newRig(t, t.TempDir())
	assert.Nil(t, r.m.preDeploy, "the command was never set")
	r.m.SetPreDeploymentCommand("")
	assert.Nil(t, r.m.preDeploy, "an empty command installs no stage")
	r.start(t)
	// submitAdmitted is the call under test: the tail's gate is not.
	r.m.submitAdmitted(mainGeneration("m1", "/nix/store/m1"), "switch")
	r.waitDeploys(t, "m1/switch")
	assert.False(t, r.m.preDeployBusy())
}

func TestPreDeployDeployFollowsExitZeroCommand(t *testing.T) {
	r, h := newHookRig(t)
	h.set(t, "block-gen-m1")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m1", "/nix/store/m1")))
	h.waitStarts(t, "gen-m1", 1)
	require.Never(t, func() bool { return len(r.deployLog()) > 0 }, 300*time.Millisecond, 10*time.Millisecond, "deployed before the command exited")
	h.clear("block-gen-m1")
	r.waitDeploys(t, "m1/switch")
	assert.Equal(t, []string{"0"}, h.exits("gen-m1"))

	// The deferred generation goes through the command too.
	h.set(t, "block-gen-m2")
	r.forceDeferred(evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100))
	pollNow(r)
	h.waitStarts(t, "gen-m2", 1)
	require.Never(t, func() bool { return len(r.deployLog()) > 1 }, 300*time.Millisecond, 10*time.Millisecond, "the deferred generation deployed before the command exited")
	h.clear("block-gen-m2")
	r.waitDeploys(t, "m1/switch", "m2/switch")
	assert.Equal(t, []string{"0"}, h.exits("gen-m2"))
}

func TestPreDeployGateRefusalStartsNoCommand(t *testing.T) {
	remote := newGitRemote(t)
	tip := remote.commit("", "m1")
	remote.setBranch("main", tip)
	dir := t.TempDir()
	r := newGitRig(t, dir, remote)
	h := installHook(t, r.m, dir)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	assert.Equal(t, []string{"0"}, h.exits(tip))

	r.writeLease(t, "session")
	tip = remote.commit(tip, "m2")
	remote.setBranch("main", tip)
	r.fetch(t, "m2")
	r.waitMainDecided(t, tip)
	require.Equal(t, tip, r.deferredCommit())
	assert.Equal(t, 0, h.starts(tip), "the gate refused m2")

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "m2/switch")
	assert.Equal(t, []string{"0"}, h.exits(tip))
}

func TestPreDeployLeaseWrittenDuringCommandDefersThenRerunsCommand(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "block-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m2", "/nix/store/m2")))
	h.waitStarts(t, "gen-m2", 1)
	r.writeLease(t, "session")
	h.clear("block-gen-m2")
	h.waitExits(t, "gen-m2", "0")
	waitFor(t, r.stageIdle, "the stage is idle")
	assert.Equal(t, "gen-m2", r.deferredUuid())
	assert.Equal(t, []string{"m1/switch"}, r.deployLog())

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "m2/switch")
	assert.Equal(t, []string{"0", "0"}, h.exits("gen-m2"), "the deferred generation ran the command again")
}

// m3 is staged under a session lease while m4, evaluated after it, is deferred.
func TestPreDeployNewerDeferredDropsStagedMain(t *testing.T) {
	for _, leaseEndsBeforeExit := range []bool{false, true} {
		name := "lease ends after the command exits"
		if leaseEndsBeforeExit {
			name = "lease ends before the command exits"
		}
		t.Run(name, func(t *testing.T) {
			r, h := newHookRig(t)
			hookDeployMain(t, r, "m1")
			h.set(t, "block-gen-m3")
			require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 100)))
			h.waitStarts(t, "gen-m3", 1)
			r.writeLease(t, "session")
			require.Equal(t, "skipped:lease-decision", hookTail(r, evalAt(mainGeneration("m4", "/nix/store/m4"), "gen-m4", 200)))
			require.Equal(t, "gen-m4", r.deferredUuid())

			if leaseEndsBeforeExit {
				require.NoError(t, os.Remove(r.lease))
			}
			h.clear("block-gen-m3")
			h.waitExits(t, "gen-m3", "0")
			if !leaseEndsBeforeExit {
				waitFor(t, r.stageIdle, "the stage is idle")
				assert.Equal(t, []string{"m1/switch"}, r.deployLog())
				assert.Equal(t, "gen-m4", r.deferredUuid())
				require.NoError(t, os.Remove(r.lease))
			}
			r.waitDeploys(t, "m1/switch", "m4/switch")
			assert.Equal(t, 1, h.starts("gen-m3"))
		})
	}
}

// t2 is staged under a git lease while the tier's m4 is deferred; the
// fetcher never selects t2 again, so it deploys.
func TestPreDeployTestingGenerationIgnoresNewerDeferred(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t1", "m1")))
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists(), "the post-deployment command took a git lease")

	h.set(t, "block-gen-t2")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t2", "m1")))
	h.waitStarts(t, "gen-t2", 1)
	require.Equal(t, "skipped:lease-decision", hookTail(r, evalAt(mainGeneration("m4", "/nix/store/m4"), "gen-m4", 200)))
	require.Equal(t, "gen-m4", r.deferredUuid())
	h.clear("block-gen-t2")
	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test")
	assert.Equal(t, "gen-m4", r.deferredUuid())
}

func TestPreDeployFailedMainIsDeferredAndRerun(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "failonce-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m2", "/nix/store/m2")))
	r.waitDeploys(t, "m1/switch", "m2/switch")
	assert.Equal(t, []string{"1", "0"}, h.exits("gen-m2"), "two runs, the second after the first failed")
}

// The failed m2 is older than the deferred m3: m3 stays, m2 never deploys.
func TestPreDeployFailedMainKeepsNewerDeferred(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "block-gen-m2")
	h.set(t, "failonce-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	h.waitStarts(t, "gen-m2", 1)
	r.writeLease(t, "session")
	require.Equal(t, "skipped:lease-decision", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	h.clear("block-gen-m2")
	h.waitExits(t, "gen-m2", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	assert.Equal(t, "gen-m3", r.deferredUuid())

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "m3/switch")
	assert.Equal(t, 1, h.starts("gen-m2"))
}

func TestPreDeployFailedTestingIsDroppedAndLogged(t *testing.T) {
	logs := logtest.NewGlobal()
	old := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	logrus.AddHook(logs)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(old) })

	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t1", "m1")))
	r.waitDeploys(t, "m1/switch", "t1/test")

	g := testingGeneration("t2", "m1")
	g.SelectedCommitId = "deadbeef0001"
	h.set(t, "fail-gen-t2")
	require.Equal(t, "submitted:test", hookTail(r, g))
	h.waitExits(t, "gen-t2", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	assert.Equal(t, "", r.deferredUuid(), "a failed testing generation is not deferred")
	named := slices.ContainsFunc(logs.AllEntries(), func(e *logrus.Entry) bool {
		return e.Level == logrus.ErrorLevel && strings.Contains(e.Message, "gen-t2") && strings.Contains(e.Message, "deadbeef0001")
	})
	assert.True(t, named, "an error log names the generation and its commit")

	require.NoError(t, os.Remove(r.lease))
	pollNow(r)
	require.Never(t, func() bool { return h.starts("gen-t2") > 1 || len(r.deployLog()) > 2 }, 300*time.Millisecond, 10*time.Millisecond)
}

func TestPreDeployDisabledLeaseReaderRerunsFailedMain(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.m.leaseReader = lease.NewReader("")
	h := installHook(t, r.m, dir)
	r.start(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "failonce-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m2", "/nix/store/m2")))
	r.waitDeploys(t, "m1/switch", "m2/switch")
	assert.Equal(t, []string{"1", "0"}, h.exits("gen-m2"))
}

// The disabled gate admits m3 without clearing the older failed m2.
func TestPreDeployDisabledLeaseReaderNewerGenerationSupersedesFailedMain(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.m.leaseReader = lease.NewReader("")
	r.m.SetPollPeriod(time.Hour)
	h := installHook(t, r.m, dir)
	r.start(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "failonce-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	h.waitExits(t, "gen-m2", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	require.Equal(t, "gen-m2", r.deferredUuid())

	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	assert.Equal(t, "", r.deferredUuid(), "m3 supersedes the failed m2")
	r.waitDeploys(t, "m1/switch", "m3/switch")
	pollNow(r)
	require.Never(t, func() bool { return len(r.deployLog()) > 2 }, 300*time.Millisecond, 10*time.Millisecond, "the older m2 deployed over m3")
}

// newQueuedRig: m1 ran, m1b is deploying (held), m2 is queued in the
// deployer and m3 is in the stage with its command blocked.
func newQueuedRig(t *testing.T) (*rig, *hookCtl, chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	r := newRig(t, dir)
	r.m.SetPollPeriod(time.Hour)
	h := installHook(t, r.m, dir)
	r.start(t)
	hookDeployMain(t, r, "m1")
	gate := r.holdDeploys()
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m1b", "/nix/store/m1b")))
	waitFor(t, func() bool { return slices.Contains(r.deployLog(), "m1b/switch") }, "m1b in flight")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	waitFor(t, func() bool { _, queued := r.d.Activity(); return queued }, "m2 queued")
	return r, h, gate
}

// With m3 in the stage the older deferred m2 must not be offered over it.
func TestPreDeployDeferredOfferWaitsForBusyStage(t *testing.T) {
	r, h, gate := newQueuedRig(t)
	h.set(t, "block-gen-m3")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	h.waitStarts(t, "gen-m3", 1)
	r.writeLease(t, "closure")
	r.releaseDeploys(gate)
	waitFor(t, func() bool { return r.d.Idle() && r.processed() && r.deferredUuid() == "gen-m2" }, "m2 refused at take and deferred")

	require.NoError(t, os.Remove(r.lease))
	pollNow(r)
	h.clear("block-gen-m3")
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m3/switch")
	assert.Equal(t, 1, h.starts("gen-m2"), "m2 ran its command once, before it was queued")
}

// m3's command fails while m1b runs and m2 waits: m3 stays deferred
// through m2's admission and runs again once the deployer is idle.
func TestPreDeployFailedMainSurvivesOlderQueuedAdmission(t *testing.T) {
	r, h, gate := newQueuedRig(t)
	h.set(t, "failonce-gen-m3")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	h.waitExits(t, "gen-m3", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	require.Equal(t, "gen-m3", r.deferredUuid())

	r.releaseDeploys(gate)
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m2/switch", "m3/switch")
	assert.Equal(t, []string{"1", "0"}, h.exits("gen-m3"))
}

// The same failure, then a closure lease makes the deployer refuse m2 at take.
func TestPreDeployFailedMainSurvivesOlderQueuedRefusal(t *testing.T) {
	r, h, gate := newQueuedRig(t)
	h.set(t, "failonce-gen-m3")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	h.waitExits(t, "gen-m3", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	r.writeLease(t, "closure")
	r.releaseDeploys(gate)
	waitFor(t, func() bool { return r.d.Idle() && r.processed() }, "m2 refused at take")
	r.quiesce()
	assert.Equal(t, "gen-m3", r.deferredUuid())
	assert.Equal(t, []string{"m1/switch", "m1b/switch"}, r.deployLog())

	require.NoError(t, os.Remove(r.lease))
	pollNow(r)
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m3/switch")
	assert.Equal(t, []string{"1", "0"}, h.exits("gen-m3"))
}

func TestPreDeploySwitchLatestBypassesStage(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	h.set(t, "block-gen-m2")
	require.Equal(t, "submitted:switch", hookTail(r, mainGeneration("m2", "/nix/store/m2")))
	h.waitStarts(t, "gen-m2", 1)

	require.NoError(t, r.m.SwitchDeploymentLatest())
	require.Eventually(t, func() bool {
		return slices.Equal(r.deployLog(), []string{"m1/switch", "m1/switch"})
	}, 5*time.Second, 5*time.Millisecond, "switch-latest deployed before m2's command exited, got %v", r.deployLog())
	assert.Equal(t, 1, h.starts("gen-m1"), "switch-latest ran no command")
	h.clear("block-gen-m2")
	r.waitDeploys(t, "m1/switch", "m1/switch", "m2/switch")
}

// Through the fetcher, builder and confirmers: m2 is built and confirmed
// while m1's command runs, and only m2 deploys.
func TestPreDeployNewGenerationIsPendingWhileCommandRuns(t *testing.T) {
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

	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	r.fetch(t, "m2")
	waitFor(t, func() bool {
		st := r.m.preDeploy.Status()
		return st.Running != nil && st.Pending != nil && st.Pending.Generation.SelectedCommitId == m2
	}, "m2 pending behind m1")
	assert.Equal(t, m1, r.m.preDeploy.Status().Running.Generation.SelectedCommitId)
	assert.Empty(t, r.deployLog())

	h.clear("block-all")
	r.waitDeploys(t, "m2/switch")
	assert.Equal(t, []string{"0"}, h.exits(m1))
	assert.Equal(t, []string{"0"}, h.exits(m2))
	assert.Equal(t, 1, h.starts(m1))
}

// m2, refused at take, is deferred when m3's command fails: m3 is newer and replaces it.
func TestPreDeployFailedMainReplacesOlderDeferred(t *testing.T) {
	r, h, gate := newQueuedRig(t)
	h.set(t, "block-gen-m3")
	h.set(t, "failonce-gen-m3")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	h.waitStarts(t, "gen-m3", 1)
	r.writeLease(t, "closure")
	r.releaseDeploys(gate)
	waitFor(t, func() bool { return r.d.Idle() && r.processed() && r.deferredUuid() == "gen-m2" }, "m2 refused at take and deferred")
	require.NoError(t, os.Remove(r.lease))
	h.clear("block-gen-m3")
	h.waitExits(t, "gen-m3", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	assert.Equal(t, "gen-m3", r.deferredUuid())

	pollNow(r)
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m3/switch")
	assert.Equal(t, []string{"1", "0"}, h.exits("gen-m3"))
}

func newDisabledReaderHookRig(t *testing.T) (*rig, *hookCtl) {
	t.Helper()
	dir := t.TempDir()
	r := newRig(t, dir)
	r.m.leaseReader = lease.NewReader("")
	r.m.SetPollPeriod(time.Hour)
	h := installHook(t, r.m, dir)
	r.start(t)
	return r, h
}

// t1 goes through the stage; quiesce lets m0's post-deployment poll finish first.
func TestPreDeployDisabledLeaseReaderAdmittedTestingSupersedesFailedMain(t *testing.T) {
	r, h := newDisabledReaderHookRig(t)
	hookDeployMain(t, r, "m0")
	r.quiesce()
	h.set(t, "failonce-gen-m1")
	require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m1", "/nix/store/m1"), "gen-m1", 100)))
	h.waitExits(t, "gen-m1", "1")
	waitFor(t, r.stageIdle, "the stage is idle")
	require.Equal(t, "gen-m1", r.deferredUuid())

	require.Equal(t, "submitted:test", hookTail(r, evalAt(testingGeneration("t1", "m0"), "gen-t1", 200)))
	r.waitDeploys(t, "m0/switch", "t1/test")
	pollNow(r)
	require.Never(t, func() bool { return h.starts("gen-m1") > 1 || len(r.deployLog()) > 2 }, 300*time.Millisecond, 10*time.Millisecond, "the failed m1 was offered again after t1")
	assert.Equal(t, "", r.deferredUuid())
}

// pauseLog blocks, once, the goroutine that logs a message containing substr.
type pauseLog struct {
	substr  string
	once    sync.Once
	reached chan struct{}
	release chan struct{}
	unblock func()
}

func pauseOnLog(t *testing.T, substr string) *pauseLog {
	t.Helper()
	p := &pauseLog{substr: substr, reached: make(chan struct{}), release: make(chan struct{})}
	var released sync.Once
	p.unblock = func() { released.Do(func() { close(p.release) }) }
	old := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	logrus.AddHook(p)
	t.Cleanup(func() {
		p.unblock()
		logrus.StandardLogger().ReplaceHooks(old)
	})
	return p
}

func (p *pauseLog) Levels() []logrus.Level { return logrus.AllLevels }

func (p *pauseLog) Fire(e *logrus.Entry) error {
	if strings.Contains(e.Message, p.substr) {
		p.once.Do(func() {
			close(p.reached)
			<-p.release
		})
	}
	return nil
}

// m2 (older) fails while m3 is offered; with the reader disabled only m3 may deploy.
func TestPreDeployDisabledLeaseReaderNewerOfferDuringFailure(t *testing.T) {
	// m3 is Pending when m2's command exits 1: the stage supersedes m2 and hookFailed never runs.
	t.Run("pending when the command fails", func(t *testing.T) {
		r, h := newDisabledReaderHookRig(t)
		hookDeployMain(t, r, "m1")
		r.quiesce()
		h.set(t, "block-gen-m2")
		h.set(t, "failonce-gen-m2")
		require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
		h.waitStarts(t, "gen-m2", 1)
		require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
		require.Equal(t, "gen-m3", r.m.preDeploy.Status().Pending.Generation.Uuid)
		h.clear("block-gen-m2")
		r.waitDeploys(t, "m1/switch", "m3/switch")
		pollNow(r)
		require.Never(t, func() bool { return h.starts("gen-m2") > 1 || len(r.deployLog()) > 2 }, 300*time.Millisecond, 10*time.Millisecond, "m2 was offered again after m3")
		assert.Equal(t, "", r.deferredUuid())
	})
	// m3 is offered inside hookFailed(m2), after the stage found no Pending and before m2 is deferred.
	t.Run("offered while the failure is recorded", func(t *testing.T) {
		r, h := newDisabledReaderHookRig(t)
		hookDeployMain(t, r, "m1")
		r.quiesce()
		pause := pauseOnLog(t, "deferring the main generation gen-m2")
		h.set(t, "failonce-gen-m2")
		require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
		select {
		case <-pause.reached:
		case <-time.After(waitDeadline):
			t.Fatal("hookFailed never ran for m2")
		}
		require.Equal(t, "submitted:switch", hookTail(r, evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
		st := r.m.preDeploy.Status()
		require.Equal(t, "gen-m2", st.Running.Generation.Uuid)
		require.Equal(t, "gen-m3", st.Pending.Generation.Uuid)
		pause.unblock()
		r.waitDeploys(t, "m1/switch", "m3/switch")
		pollNow(r)
		require.Never(t, func() bool { return h.starts("gen-m2") > 1 || len(r.deployLog()) > 2 }, 300*time.Millisecond, 10*time.Millisecond, "m2 was offered again after m3")
		assert.Equal(t, "", r.deferredUuid())
	})
}

// Under a git lease the testing t2, evaluated after the frozen m2, leaves m2 deferred.
func TestPreDeployTestingAdmissionKeepsFrozenMainUnderLease(t *testing.T) {
	r, h := newHookRig(t)
	hookDeployMain(t, r, "m1")
	require.Equal(t, "submitted:test", hookTail(r, testingGeneration("t1", "m1")))
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists(), "the post-deployment command took a git lease")
	require.Equal(t, "skipped:lease-decision", hookTail(r, evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	require.Equal(t, "gen-m2", r.deferredUuid())

	require.Equal(t, "submitted:test", hookTail(r, evalAt(testingGeneration("t2", "m1"), "gen-t2", 200)))
	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test")
	assert.Equal(t, []string{"0"}, h.exits("gen-t2"))
	assert.Equal(t, "gen-m2", r.deferredUuid())

	r.exec.set("/nix/store/m1")
	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "t1/test", "t2/test", "m2/switch")
	assert.Equal(t, []string{"0"}, h.exits("gen-m2"))
}
