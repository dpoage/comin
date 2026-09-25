package manager

// This file holds the manager's override-lease-aware decisions: the deploy
// gate (C1 tier freeze, C2 kind gate and descent check, released heads, and
// the rule that comin only deploys over a system it owns), applied when a
// generation is confirmed and again when the deployer is about to start
// it; the tier generation the gate defers; releasing the testing
// deployments of an ended override (C3), which never deploys anything; and
// the S4 drift status (C5), which shares its "expected deployment" query
// with the gate.

import (
	"slices"
	"time"

	"github.com/nlewo/comin/internal/lease"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxInFlightDeployAge bounds how long an in-flight deployment counts
// towards the "returning" drift state. switch-to-configuration runs with no
// timeout, so a hung activation must eventually be reported as leaseless
// drift rather than "returning" forever.
const maxInFlightDeployAge = 30 * time.Minute

// isTestingGeneration reports whether g is a testing-branch generation. It
// uses Generation.SelectedBranchIsTesting rather than store.IsTesting
// (which keys on the deployment's operation): a switch-latest redeploy of a
// testing head with operation "switch" must still count as testing.
func isTestingGeneration(g *protobuf.Generation) bool {
	return g.GetSelectedBranchIsTesting().GetValue()
}

func isTestingDeployment(d *protobuf.Deployment) bool {
	return isTestingGeneration(d.GetGeneration())
}

// heldMainCommitId returns the MainCommitId of the last done, non-testing
// deployment: the main commit that is actually running and, while a lease
// is present (C1), cannot advance any further.
func heldMainCommitId(deployments []*protobuf.Deployment) string {
	latest := latestDeployment(deployments, func(d *protobuf.Deployment) bool {
		return !isTestingDeployment(d)
	})
	if latest == nil {
		return ""
	}
	return latest.Generation.GetMainCommitId()
}

// expectedDeployment is the S4/C7 "expected" deployment: the last DONE
// deployment that is not released and non-testing, except that testing
// deployments count too while a lease of kind git exists (countTesting).
func (m *Manager) expectedDeployment(deployments []*protobuf.Deployment, countTesting bool) *protobuf.Deployment {
	return latestDeployment(deployments, func(d *protobuf.Deployment) bool {
		if m.leaseReader.Enabled() && m.leaseState.IsReleased(d.Uuid) {
			return false
		}
		return countTesting || !isTestingDeployment(d)
	})
}

// latestDeployment returns the Done deployment satisfying keep with the
// most recent EndedAt. It picks by EndedAt rather than trusting the list
// order: a deployment's Status flips to Done before the manager's
// DeploymentDoneCh handler re-inserts it at the front of the list.
func latestDeployment(deployments []*protobuf.Deployment, keep func(*protobuf.Deployment) bool) *protobuf.Deployment {
	var latest *protobuf.Deployment
	for _, d := range deployments {
		if d.Status != store.StatusToString(store.Done) {
			continue
		}
		if !keep(d) {
			continue
		}
		if latest == nil || d.GetEndedAt().AsTime().After(latest.GetEndedAt().AsTime()) {
			latest = d
		}
	}
	return latest
}

// ownsRunningSystem reports whether /run/current-system is a system comin
// may deploy over. comin owns it when nothing is expected yet (a freshly
// installed machine), when it is the S4 expected system, or when it is the
// system of comin's latest deployment (in flight, or failed after the
// activation repointed it) and that deployment is not an ended override
// (endedOverride). Anything
// else is not comin's: the testing system of an ended override, a
// break-glass or closure activated out of band, a rollback done by another
// tool. comin leaves such a system as it is, and S4 reports it, until it is
// the expected one again (a reboot back to it, or `switch-latest`). When
// it is not owned, the reason is returned for the log.
func (m *Manager) ownsRunningSystem(obs lease.Observation) (owned bool, reason string) {
	expected := m.expectedDeployment(m.storage.DeploymentList(), obs.IsGit())
	if expected == nil {
		return true, ""
	}
	current, err := m.executor.CurrentSystem()
	if err != nil {
		return false, "the current system cannot be read: " + err.Error()
	}
	if current == expected.Generation.GetOutPath() {
		return true, ""
	}
	if latest := m.deployer.Deployment(); latest != nil && !m.endedOverride(latest) && current == latest.Generation.GetOutPath() {
		return true, ""
	}
	return false, "the running system " + current + " is neither the expected " + expected.Generation.GetOutPath() + " nor comin's latest deployment"
}

// releasePending reports whether an override ended but C3 has not
// recorded its release yet: no lease, and a lease-aware testing
// deployment is still unreleased.
func (m *Manager) releasePending(obs lease.Observation) bool {
	if obs.Exists {
		return false
	}
	for _, d := range m.storage.DeploymentList() {
		if isTestingDeployment(d) && m.leaseState.IsLeaseAware(d.Uuid) && !m.leaseState.IsReleased(d.Uuid) {
			return true
		}
	}
	return false
}

// leaseDeployDecision is the deploy gate. It runs when a built generation
// is confirmed, again when the deployer is about to start it (see
// admitQueued), so a change while the generation is queued still stops
// it, and when the deferred generation is offered. With the lease reader
// disabled (overrideLeaseFile is null) it always admits: the decisions are
// be6025e's. A refused main generation is deferred (offerDeferred); a
// refused testing generation is dropped.
//   - C1: while a lease exists no main generation deploys.
//   - After an override ended and before C3 recorded its release, nothing
//     deploys: the ended override's head must be released before any
//     decision can admit it.
//   - C2: a lease of any kind but git refuses a testing head. Under a git
//     lease the held main commit must be an ancestor of the testing head.
//     With no lease there is no descent check: the repository already
//     requires the testing head to descend from the fetched main head.
//   - A testing head that is the commit of a released deployment of the
//     same branch never deploys again.
//   - Nothing deploys over a system comin does not own (ownsRunningSystem).
//
// switch-latest requests never pass through it.
func (m *Manager) leaseDeployDecision(g *protobuf.Generation) (operation string, ok bool) {
	operation = m.getOperationFromConfigurationOperations(g.SelectedRemoteName, g.SelectedBranchName)
	if !m.leaseReader.Enabled() {
		return operation, true
	}
	obs := m.leaseReader.Observe()

	if !isTestingGeneration(g) {
		if obs.Exists {
			logrus.Infof("manager: deferring the main generation %s: an override lease is held", g.Uuid)
			m.setDeferred(g)
			return operation, false
		}
		if m.releasePending(obs) {
			logrus.Infof("manager: deferring the main generation %s: the ended override is not released yet", g.Uuid)
			m.setDeferred(g)
			return operation, false
		}
		if owned, reason := m.ownsRunningSystem(obs); !owned {
			logrus.Infof("manager: deferring the main generation %s: %s", g.Uuid, reason)
			m.setDeferred(g)
			return operation, false
		}
		m.setDeferred(nil)
		return operation, true
	}

	if obs.Exists && !obs.IsGit() {
		logrus.Infof("manager: skipping deployment of the testing generation %s: the override lease is not of kind git", g.Uuid)
		return operation, false
	}
	deployments := m.storage.DeploymentList()
	if m.releasedHeads(deployments)[testingHead(g)] {
		logrus.Infof("manager: skipping deployment of the testing generation %s: its commit %s is the head of an ended override", g.Uuid, g.SelectedCommitId)
		return operation, false
	}
	if m.releasePending(obs) {
		logrus.Infof("manager: skipping deployment of the testing generation %s: the ended override is not released yet", g.Uuid)
		return operation, false
	}
	if owned, reason := m.ownsRunningSystem(obs); !owned {
		logrus.Infof("manager: skipping deployment of the testing generation %s: %s", g.Uuid, reason)
		return operation, false
	}
	if obs.IsGit() {
		if held := heldMainCommitId(deployments); held != "" && !m.descendsFrom(g, held) {
			logrus.Infof("manager: skipping deployment of the testing generation %s: it does not descend from the held main commit %s", g.Uuid, held)
			return operation, false
		}
	}
	return operation, true
}

// admitQueued is the deployer's admission func: the deploy gate, applied
// again right before a queued generation starts.
func (m *Manager) admitQueued(g *protobuf.Generation) bool {
	_, ok := m.leaseDeployDecision(g)
	return ok
}

// descendsFrom reports whether held is the testing head g or one of its
// ancestors. The repository only selects a testing head that descends from
// the main commit it carries, so g.MainCommitId == held settles it without
// asking git.
func (m *Manager) descendsFrom(g *protobuf.Generation, held string) bool {
	if g.GetMainCommitId() == held {
		return true
	}
	ok, err := m.Fetcher.IsAncestor(held, g.GetSelectedCommitId())
	if err != nil {
		logrus.Errorf("manager: could not check that %s descends from %s: %s", g.GetSelectedCommitId(), held, err)
		return false
	}
	return ok
}

// testingHeadKey names the head of a testing branch.
type testingHeadKey struct {
	remote, branch, commit string
}

func testingHead(g *protobuf.Generation) testingHeadKey {
	return testingHeadKey{remote: g.GetSelectedRemoteName(), branch: g.GetSelectedBranchName(), commit: g.GetSelectedCommitId()}
}

// endedOverride reports whether d is a testing deployment of an ended
// override: released, or, after the lease state was lost (MarksLostAt),
// ended before the loss, since its marks are unknown.
func (m *Manager) endedOverride(d *protobuf.Deployment) bool {
	if !isTestingDeployment(d) {
		return false
	}
	if m.leaseState.IsReleased(d.Uuid) {
		return true
	}
	lostAt := m.leaseState.MarksLostAt()
	return lostAt != nil && d.EndedAt != nil && d.EndedAt.AsTime().Before(*lostAt)
}

// releasedHeads returns the heads of ended overrides, per remote and
// branch. The fetcher keeps selecting such a head while it is still the
// branch head, re-emits it after a restart, and a remote branch deleted
// at override end stays in comin's local repository, so without this an
// ended override would come back.
func (m *Manager) releasedHeads(deployments []*protobuf.Deployment) map[testingHeadKey]bool {
	heads := map[testingHeadKey]bool{}
	for _, d := range deployments {
		if m.endedOverride(d) {
			heads[testingHead(d.Generation)] = true
		}
	}
	return heads
}

func (m *Manager) setDeferred(g *protobuf.Generation) {
	m.deferredMu.Lock()
	defer m.deferredMu.Unlock()
	m.deferred = g
}

func (m *Manager) takeDeferred() *protobuf.Generation {
	m.deferredMu.Lock()
	defer m.deferredMu.Unlock()
	g := m.deferred
	m.deferred = nil
	return g
}

// offerDeferred offers the main generation the gate deferred, once there
// is no lease, the deployer is idle and comin owns the running system
// again (after a reboot back to the expected system, or once a
// switch-latest has activated it). Until then the generation stays
// deferred. It goes through the deploy gate and Submit like a freshly
// confirmed generation.
func (m *Manager) offerDeferred() {
	if !m.leaseReader.Enabled() {
		return
	}
	obs := m.leaseReader.Observe()
	if obs.Exists || !m.deployer.Idle() {
		return
	}
	if owned, _ := m.ownsRunningSystem(obs); !owned {
		return
	}
	g := m.takeDeferred()
	if g == nil {
		return
	}
	operation, ok := m.leaseDeployDecision(g)
	if !ok {
		return
	}
	logrus.Infof("manager: deploying the deferred main generation %s", g.Uuid)
	m.deployer.Submit(g, operation)
}

// releaseEndedOverrides implements C3: when there is no lease file and
// nothing is queued or in flight (the post-deployment command, which
// takes the lease, has returned), every lease-aware testing deployment not
// released yet is recorded released, in one write. A released deployment
// is never S4 expected, and its head never deploys again. Nothing is
// deployed: returning the machine to its tier is the operator's
// `switch-latest`. Testing deployments made by a pre-fork comin carry no
// lease-aware mark and are never released.
func (m *Manager) releaseEndedOverrides() {
	if !m.leaseReader.Enabled() || m.leaseReader.Observe().Exists || !m.deployer.Idle() {
		return
	}
	// The store can hold several rows of one deployment.
	var release []string
	for _, d := range m.storage.DeploymentList() {
		if isTestingDeployment(d) && m.leaseState.IsLeaseAware(d.Uuid) && !m.leaseState.IsReleased(d.Uuid) && !slices.Contains(release, d.Uuid) {
			release = append(release, d.Uuid)
		}
	}
	if len(release) == 0 {
		return
	}
	if err := m.leaseState.Release(release); err != nil {
		logrus.Errorf("manager: could not record the end of the override: %s", err)
		return
	}
	logrus.Infof("manager: the override ended; its testing deployments %v are released and the running system is left as it is", release)
}

// poll re-evaluates everything that can change without a fetch: C3, the
// deferred tier generation, and the S4 drift episode (so `since` is
// recorded even if nobody queries the status).
func (m *Manager) poll(now time.Time) {
	m.releaseEndedOverrides()
	m.offerDeferred()
	m.driftStatus(now)
}

// driftInputs is the pure decision surface for S4's state machine, kept
// separate from the I/O that gathers it (executor reads, store scans,
// persisted "since") so the precedence rules are unit-testable without a
// live deployer or clock.
type driftInputs struct {
	leaseExists           bool
	hasExpected           bool
	currentEqualsExpected bool
	inFlight              bool
	inFlightAge           time.Duration
	queued                bool
}

// decideDriftState applies S4's precedence: held (lease exists) beats none
// (already at expected, or nothing expected) beats returning (work queued,
// a switch-latest request included, or an in-flight deploy under
// maxInFlightDeployAge) beats leaseless.
func decideDriftState(in driftInputs) string {
	if in.leaseExists {
		return "held"
	}
	if in.currentEqualsExpected || !in.hasExpected {
		return "none"
	}
	inFlightCounts := in.inFlight && in.inFlightAge < maxInFlightDeployAge
	if inFlightCounts || in.queued {
		return "returning"
	}
	return "leaseless"
}

// inFlightAge is how long the deployment in flight has been running.
func (m *Manager) inFlightAge(now time.Time) time.Duration {
	dpl := m.deployer.Deployment()
	if dpl == nil || dpl.StartedAt == nil {
		return 0
	}
	return now.Sub(dpl.StartedAt.AsTime())
}

// driftStatus computes S4's `drift` object and persists the start of the
// current drift episode ("since") the first time it is observed, clearing
// it once the state is back to none. toState calls it for `comin status`,
// and poll calls it on every tick and after every finished deployment.
func (m *Manager) driftStatus(now time.Time) *protobuf.Drift {
	// The deployer's activity is read first: when it reports nothing
	// queued or in flight, every deployment it made has finished, so
	// the store and the current system read below already show it.
	inFlight, queued := m.deployer.Activity()
	var inFlightAge time.Duration
	if inFlight {
		inFlightAge = m.inFlightAge(now)
	}
	obs := m.leaseReader.Observe()
	expected := m.expectedDeployment(m.storage.DeploymentList(), obs.IsGit())
	hasExpected := expected != nil
	var expectedOutPath string
	if hasExpected {
		expectedOutPath = expected.Generation.GetOutPath()
	}
	current, err := m.executor.CurrentSystem()
	if err != nil {
		logrus.Errorf("manager: could not read the current system: %s", err)
	}

	state := decideDriftState(driftInputs{
		leaseExists:           obs.Exists,
		hasExpected:           hasExpected,
		currentEqualsExpected: hasExpected && current == expectedOutPath,
		inFlight:              inFlight,
		inFlightAge:           inFlightAge,
		queued:                queued,
	})

	var since *time.Time
	if state == "none" {
		if err := m.leaseState.SetDriftSince(nil); err != nil {
			logrus.Errorf("manager: could not clear drift-since: %s", err)
		}
	} else {
		since = m.leaseState.DriftSince()
		if since == nil {
			t := now
			since = &t
			if err := m.leaseState.SetDriftSince(&t); err != nil {
				logrus.Errorf("manager: could not persist drift-since: %s", err)
			}
		}
	}

	d := &protobuf.Drift{
		ExpectedOutPath: expectedOutPath,
		State:           state,
	}
	if since != nil {
		d.Since = timestamppb.New(*since)
	}
	return d
}
