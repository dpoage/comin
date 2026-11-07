package manager

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/reconcile"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/syncgate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *rig) waitFor(what string, f func() bool) {
	r.t.Helper()
	require.Eventually(r.t, f, 15*time.Second, 10*time.Millisecond, what)
}

func (r *rig) hasCandidates() bool {
	return len(repository.Candidates(r.m.Fetcher.Latest())) > 0
}

func (r *rig) reconcileState() (state, reason string) {
	rc := r.m.GetState().GetReconcile()
	return rc.GetState(), rc.GetReason()
}

// nothingRan asserts that no evaluation, build, ask or deployment happened.
func (r *rig) nothingRan() {
	r.t.Helper()
	assert.Empty(r.t, r.exec.evalLog(), "evaluations")
	assert.Zero(r.t, r.exec.buildCount(), "builds")
	assert.Empty(r.t, r.asks(), "asks")
	assert.Empty(r.t, r.exec.deployLog(), "deployments")
}

func (r *rig) building() bool   { return r.m.Builder.IsBuilding() }
func (r *rig) evaluating() bool { return r.m.Builder.IsEvaluating() }
func (r *rig) idle() bool       { return !r.building() && !r.evaluating() }

func TestSyncGateFetch(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true})
	r.gateSet("fetch", true)
	r.start()
	r.settle(15)

	assert.False(t, r.hasCandidates(), "a fetch ran under a closed gate")
	for _, remote := range r.m.Fetcher.Latest().Remotes {
		assert.Empty(t, remote.FetchErrorMsg, "a skipped fetch is not a fetch error")
	}
	r.nothingRan()
	state, reason := r.reconcileState()
	assert.Equal(t, reconcile.StateWaiting, state)
	assert.Equal(t, reconcile.ReasonNoCandidate, reason)
	assert.Positive(t, r.gatePairs()["fetch poll"], "the fetch was not asked: %v", r.gatePairs())

	r.gateSet("fetch", false)
	r.fetch()
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
}

func TestSyncGatePassClosed(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true})
	r.gateSet("pass", true)
	r.start()
	r.waitFor("a fetch", r.hasCandidates)
	r.settle(15)

	r.nothingRan()
	assert.Empty(t, r.m.DeployConfirmer.status().Submitted)
	assert.Empty(t, r.m.BuildConfirmer.status().Submitted)
	state, reason := r.reconcileState()
	assert.Equal(t, reconcile.StateWaiting, state)
	assert.Equal(t, "sync gate: "+gateReason, reason)
	assert.Empty(t, outcomes(t, statusStep(t, r)), "a closed pass walks no candidate")

	r.gateSet("pass", false)
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
	r.waitFor("converged", func() bool { s, _ := r.reconcileState(); return s == reconcile.StateConverged })
}

// A deployment waiting for its confirmation is handed over only by an open pass.
func TestSyncGateHoldsEveryStep(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true, manual: true})
	r.start()
	uuid := r.waitPending("")
	asks := len(r.asks())
	require.Positive(t, asks)

	r.gateSet("pass", true)
	r.m.DeployConfirmer.Confirm(uuid)
	r.settle(10)
	assert.Empty(t, r.exec.deployLog(), "a confirmed deployment was handed over under a closed gate")
	assert.Len(t, r.asks(), asks, "an ask ran under a closed gate")

	r.gateSet("pass", false)
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
}

func TestSyncGateFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		opts       rigOpts
		prep       func(r *rig)
		reason     string
		candidates bool
	}{
		{name: "non-zero exit with a line", opts: rigOpts{gate: true},
			prep:   func(r *rig) { r.gateSet("pass", true) },
			reason: "sync gate: " + gateReason, candidates: true},
		{name: "non-zero exit without a line", opts: rigOpts{gate: true},
			prep:   func(r *rig) { r.gateSet("pass", true); r.gateFile("gate-silent", true, "") },
			reason: "sync gate: preflight: exit status 1", candidates: true},
		{name: "timeout", opts: rigOpts{gate: true, gateTimeout: 300 * time.Millisecond},
			prep:   func(r *rig) { r.gateFile("gate-sleep-pass", true, "10\n") },
			reason: "sync gate: preflight: timed out after 300ms", candidates: true},
		{name: "missing binary", opts: rigOpts{gate: true, gatePath: "/nonexistent/sync-gate"},
			reason: "sync gate: preflight: cannot start: fork/exec /nonexistent/sync-gate: no such file or directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.preflight = true
			r := newRig(t, tt.opts)
			if tt.prep != nil {
				tt.prep(r)
			}
			r.start()
			if tt.candidates {
				r.waitFor("a fetch", r.hasCandidates)
			}
			r.settle(15)

			r.nothingRan()
			state, reason := r.reconcileState()
			assert.Equal(t, reconcile.StateWaiting, state)
			assert.Equal(t, tt.reason, reason)
			for _, remote := range r.m.Fetcher.Latest().Remotes {
				assert.Empty(t, remote.FetchErrorMsg, "a gate failure is not a fetch error")
			}
		})
	}
}

// A closed watch or pass stops the running evaluation or build; the stop
// is not a failure and the first open pass runs it again.
func TestSyncGateStop(t *testing.T) {
	stopBuild := func(t *testing.T, key string, opts rigOpts) *rig {
		opts.preflight, opts.gate, opts.watchPeriod = true, true, 100*time.Millisecond
		r := newRig(t, opts)
		r.exec.set(func(e *fakeExec) { e.buildDelay = time.Minute })
		r.start()
		r.waitFor("a build", r.building)

		r.gateSet(key, true)
		r.waitFor("the build is stopped", r.idle)
		time.Sleep(500 * time.Millisecond)
		g := r.m.Builder.State().Generation
		assert.Equal(t, store.BuildInit.String(), g.BuildStatus, "a gate stop is not a build failure")
		assert.Empty(t, g.BuildErr)
		state, reason := r.reconcileState()
		assert.Equal(t, reconcile.StateWaiting, state)
		assert.Equal(t, "sync gate: "+gateReason, reason)
		assert.Empty(t, outcomes(t, statusStep(t, r)), "no candidate shows a failure")
		assert.Equal(t, 1, r.exec.buildCount())
		r.nothingDeployed(t)

		// The first open pass builds it again, with no new fetch or evaluation.
		r.exec.set(func(e *fakeExec) { e.buildDelay = 0 })
		r.gateSet(key, false)
		r.m.wake()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		assert.Len(t, r.exec.evalLog(), 1, "the generation was evaluated again")
		assert.Equal(t, 2, r.exec.buildCount())
		return r
	}
	// With no tick only the watch can stop the build, and the first closed
	// answer is the stop's.
	t.Run("a closed watch stops a build", func(t *testing.T) {
		r := stopBuild(t, "", rigOpts{tick: time.Hour})
		assert.Equal(t, "watch", r.firstClosed().stage)
	})
	t.Run("a closed pass stops a build", func(t *testing.T) {
		r := stopBuild(t, "pass", rigOpts{})
		assert.Equal(t, "pass", r.firstClosed().stage)
		for _, g := range r.gateRunsOf("watch") {
			assert.Equal(t, "open", g.answer)
		}
	})

	t.Run("a closed gate stops an evaluation", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, gate: true, watchPeriod: 100 * time.Millisecond})
		r.exec.set(func(e *fakeExec) { e.evalCtxHold = time.Minute })
		r.start()
		r.waitFor("an evaluation", r.evaluating)

		r.gateSet("", true)
		r.waitFor("the evaluation is stopped", r.idle)
		r.settle(5)
		g := r.m.Builder.State().Generation
		assert.Equal(t, store.EvalInit.String(), g.EvalStatus, "a gate stop is not an evaluation failure")
		assert.Empty(t, g.EvalErr)
		assert.Empty(t, outcomes(t, statusStep(t, r)), "no candidate shows a failure")
		assert.Len(t, r.exec.evalLog(), 1)

		r.exec.set(func(e *fakeExec) { e.evalCtxHold = 0 })
		r.gateSet("", false)
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		assert.Len(t, r.exec.evalLog(), 2, "the stopped evaluation is run again at the first open pass")
	})

	t.Run("a deployment already handed over completes and the deployer keeps working", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, gate: true, watchPeriod: 100 * time.Millisecond})
		r.exec.set(func(e *fakeExec) { e.deployDelay = 1500 * time.Millisecond })
		r.start()
		r.waitDeploys(1)
		require.True(t, r.m.deployer.IsDeploying())

		r.gateSet("", true)
		r.waitCurrent(r.out(r.mainHead()))
		assert.Len(t, r.exec.deployLog(), 1)
		r.waitFor("the deployment ends", func() bool { return !r.m.deployer.IsDeploying() })
		assert.False(t, r.m.deployer.State().GetIsSuspended().GetValue(), "the gate stop suspended the deployer")

		next := r.pushMain("file-m2")
		r.exec.set(func(e *fakeExec) { e.deployDelay = 0 })
		r.gateSet("", false)
		r.fetch()
		r.waitDeploys(2)
		r.waitCurrent(r.out(next))
	})
}

func (r *rig) nothingDeployed(t *testing.T) {
	t.Helper()
	assert.Empty(t, r.exec.deployLog(), "deployments")
}

// stopLatency runs runs stops of a running build. Each closes the gate right
// after a watch run it observed and checks that the build is gone within
// period + the closing run's duration + 100 ms.
func stopLatency(t *testing.T, period time.Duration, opts rigOpts, runs int) {
	t.Helper()
	opts.gate = true
	opts.tick = time.Hour
	r := newRig(t, opts)
	r.exec.set(func(e *fakeExec) { e.buildDelay = time.Minute })
	r.start()

	var worst time.Duration
	for i := range runs {
		r.waitFor("a build", r.building)
		seen := len(r.gateRunsOf("watch"))
		r.waitFor("a watch run", func() bool { return len(r.gateRunsOf("watch")) > seen })
		before := len(r.gateRuns())
		closedAt := time.Now()
		r.gateSet("", true)
		var stoppedAt time.Time
		deadline := time.Now().Add(period*3 + 5*time.Second)
		for time.Now().Before(deadline) {
			if r.idle() {
				stoppedAt = time.Now()
				break
			}
			time.Sleep(time.Millisecond)
		}
		require.False(t, stoppedAt.IsZero(), "run %d: the build was not stopped", i)

		var closing gateRun
		for _, g := range r.gateRuns()[before:] {
			if g.answer == "closed" {
				closing = g
				break
			}
		}
		require.NotZero(t, closing.end, "run %d: no closed gate run logged", i)
		bound := period + closing.duration() + 100*time.Millisecond
		latency := stoppedAt.Sub(closedAt)
		worst = max(worst, latency)
		assert.LessOrEqual(t, latency, bound, "run %d: stop after %s, bound %s (gate run %s)", i, latency, bound, closing.duration())
		g := r.m.Builder.State().Generation
		assert.NotEqual(t, store.BuildFailed.String(), g.BuildStatus, "run %d: the stop recorded a build failure", i)

		r.gateSet("", false)
		r.m.wake()
	}
	t.Logf("%d stops, period %s, worst latency %s", runs, period, worst)
	assert.GreaterOrEqual(t, r.exec.buildCount(), runs, "one build per run")
	assert.Len(t, r.exec.evalLog(), 1)
}

func TestSyncGateStopLatency(t *testing.T) {
	t.Run("injected period", func(t *testing.T) {
		const period = 200 * time.Millisecond
		stopLatency(t, period, rigOpts{watchPeriod: period}, 20)
	})
	t.Run("shipped period", func(t *testing.T) {
		stopLatency(t, syncgate.WatchPeriod, rigOpts{}, 4)
	})
}

func gatePair(stage, trigger string) string { return stage + " " + trigger }

func TestSyncGateStageAndTrigger(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true, watchPeriod: 100 * time.Millisecond})
	r.exec.set(func(e *fakeExec) { e.buildDelay = 800 * time.Millisecond })
	r.start()
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))
	r.settle(3)
	idle := len(r.gateRunsOf("watch"))
	r.settle(10)
	assert.Equal(t, idle, len(r.gateRunsOf("watch")), "the watch asks while no evaluation or build runs")
	pairs := r.gatePairs()
	assert.Positive(t, pairs[gatePair("fetch", "poll")], "%v", pairs)
	assert.Positive(t, pairs[gatePair("pass", "poll")], "%v", pairs)
	assert.Positive(t, pairs[gatePair("watch", "poll")], "%v", pairs)
	for k := range pairs {
		assert.NotContains(t, k, "manual", "a poll build asked with manual: %v", pairs)
	}

	r.m.Fetcher.TriggerFetchManual([]string{"origin"})
	r.waitFor("a manual fetch ask", func() bool { return r.gatePairs()[gatePair("fetch", "manual")] > 0 })

	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitFor("a manual pass ask", func() bool { return r.gatePairs()[gatePair("pass", "manual")] > 0 })
	r.settle(3)
	assert.Zero(t, r.gatePairs()[gatePair("watch", "manual")], "no build ran for the request")
}

// A gate closed for poll and open for manual lets a manual build span
// several watch checks and deploy.
func TestSyncGateManualBuild(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true, watchPeriod: 100 * time.Millisecond})
	r.gateSet("poll", true)
	r.exec.set(func(e *fakeExec) { e.buildDelay = time.Second })
	r.start()
	r.settle(5)
	assert.False(t, r.hasCandidates(), "the poll fetch ran under a closed gate")

	// comin fetch is a manual fetch.
	r.m.Fetcher.TriggerFetchManual([]string{"origin"})
	r.waitFor("a fetch", r.hasCandidates)
	r.settle(5)
	r.nothingRan()
	_, reason := r.reconcileState()
	assert.Equal(t, "sync gate: "+gateReason, reason)

	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitFor("the build", r.building)
	// The request is pending while its build runs.
	assert.Nil(t, manualEnded(t, r), "the request ended before its decisive pass")
	r.waitDeploys(1)
	r.waitCurrent(r.out(r.mainHead()))

	watches := 0
	for _, g := range r.gateRunsOf("watch") {
		assert.Equal(t, "manual", g.trigger, "a watch check of the manual build asked with poll")
		assert.Equal(t, "open", g.answer)
		watches++
	}
	assert.GreaterOrEqual(t, watches, 2, "the build did not span two watch checks")
	assert.Equal(t, 1, r.exec.buildCount(), "the manual build was stopped")
}

func manualObject(t *testing.T, r *rig) map[string]any {
	t.Helper()
	m, _ := reconcileField(t, statusStep(t, r), "manual").(map[string]any)
	return m
}

func manualEnded(t *testing.T, r *rig) any {
	t.Helper()
	m := manualObject(t, r)
	require.NotNil(t, m, "no manual object")
	return m["ended_at"]
}

func TestSyncGateManualRequestIsDecisive(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true})
	r.gateSet("pass", true)
	r.start()
	r.waitFor("a fetch", r.hasCandidates)
	r.settle(5)
	assert.Nil(t, manualObject(t, r), "no request yet")
	_, has := reconcileField(t, statusStep(t, r), "manual").(map[string]any)
	assert.False(t, has)

	before := time.Now()
	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitFor("the request ends", func() bool { m := manualObject(t, r); return m != nil && m["ended_at"] != nil })
	m := manualObject(t, r)
	assert.Equal(t, reconcile.StateWaiting, m["state"])
	assert.Equal(t, "sync gate: "+gateReason, m["reason"])
	requested, err := time.Parse(time.RFC3339Nano, m["requested_at"].(string))
	require.NoError(t, err)
	ended, err := time.Parse(time.RFC3339Nano, m["ended_at"].(string))
	require.NoError(t, err)
	assert.False(t, requested.Before(before.Add(-time.Second)))
	assert.False(t, ended.Before(requested))
	assert.Equal(t, []string{"ended_at", "reason", "requested_at", "state"}, sortedKeys(m))

	// The passes after the decisive one are poll passes.
	decisive := r.gateRunsOf("pass")
	require.NotEmpty(t, decisive)
	manualRuns := 0
	for _, g := range decisive {
		if g.trigger == "manual" {
			manualRuns++
		}
	}
	r.settle(10)
	manualAfter := 0
	for _, g := range r.gateRunsOf("pass") {
		if g.trigger == "manual" {
			manualAfter++
		}
	}
	assert.Equal(t, manualRuns, manualAfter, "a pass asked with manual after the request ended: %v", r.gateRunsOf("pass"))
	assert.Equal(t, 1, manualAfter)
	r.nothingRan()
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// The manual object reports the last request: pending while its pass is not
// decisive, then the state and reason of the decisive pass.
func TestSyncGateManualStatus(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true, gate: true, watchPeriod: 100 * time.Millisecond})
	r.gateSet("poll", true)
	r.exec.set(func(e *fakeExec) { e.buildDelay = 800 * time.Millisecond })
	r.start()
	r.m.Fetcher.TriggerFetchManual([]string{"origin"})
	r.waitFor("a fetch", r.hasCandidates)

	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitFor("the build", r.building)
	m := manualObject(t, r)
	require.NotNil(t, m)
	assert.Nil(t, m["ended_at"], "pending while its build runs")
	assert.Equal(t, reconcile.StateWaiting, m["state"])
	assert.Contains(t, m["reason"], r.mainHead())

	r.waitFor("the request ends", func() bool { return manualEnded(t, r) != nil })
	m = manualObject(t, r)
	assert.Equal(t, reconcile.StateDeploying, m["state"])
	assert.Equal(t, "", m["reason"])
	r.waitCurrent(r.out(r.mainHead()))

	// A new request replaces the object.
	first := m["requested_at"]
	r.settle(3)
	require.NoError(t, r.m.SwitchDeploymentLatest())
	r.waitFor("the second request ends", func() bool {
		m := manualObject(t, r)
		return m["requested_at"] != first && m["ended_at"] != nil
	})
	m = manualObject(t, r)
	assert.Equal(t, reconcile.StateConverged, m["state"])
}
