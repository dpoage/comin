package manager

// This file holds the manager's override-lease-aware decisions: the deploy
// gate (C1 tier freeze, C2 kind gate and descent check, released heads),
// applied when a generation is confirmed and again when the deployer is
// about to start it; the tier generation C1 defers until the lease ends;
// releasing the testing deployments of an ended override and returning to
// the expected generation (C3); and the S4 drift status (C5), which shares
// its "expected deployment" query and "would C3 fire" predicate with the
// release logic so the two can never disagree.

import (
	"time"

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

// leaseDeployDecision is the deploy gate. It runs when a built generation
// is confirmed and again when the deployer is about to start it (see
// admitQueued), so a lease that appears while the generation is queued
// still stops it. With the lease reader disabled (overrideLeaseFile is
// null) it always admits: the decisions are be6025e's.
//   - C1: while a lease exists, no main generation deploys; the refused
//     generation is deferred until the lease ends (offerDeferred).
//   - C2: a lease of any kind but git refuses a testing head. Under a git
//     lease the held main commit must be an ancestor of the testing head.
//     With no lease there is no descent check: the repository already
//     requires the testing head to descend from the fetched main head.
//   - A testing head that is the commit of a released deployment of the
//     same branch never deploys again.
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
		m.setDeferred(nil)
		return operation, true
	}

	if obs.Exists && !obs.IsGit() {
		logrus.Infof("manager: skipping deployment of the testing generation %s: the override lease is not of kind git", g.Uuid)
		return operation, false
	}
	deployments := m.storage.DeploymentList()
	if m.isReleasedHead(g, deployments) {
		logrus.Infof("manager: skipping deployment of the testing generation %s: its commit %s is the head of an ended override", g.Uuid, g.SelectedCommitId)
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

// isReleasedHead reports whether g's commit is the commit of a released
// testing deployment of the same remote and branch. The fetcher re-emits
// that head after a restart, and a remote branch deleted at override end
// stays in comin's local repository, so without this an ended override
// would come back.
func (m *Manager) isReleasedHead(g *protobuf.Generation, deployments []*protobuf.Deployment) bool {
	for _, d := range deployments {
		dg := d.GetGeneration()
		if !isTestingGeneration(dg) || dg.GetSelectedCommitId() != g.GetSelectedCommitId() {
			continue
		}
		if dg.GetSelectedRemoteName() != g.GetSelectedRemoteName() || dg.GetSelectedBranchName() != g.GetSelectedBranchName() {
			continue
		}
		if m.leaseState.IsReleased(d.Uuid) {
			return true
		}
	}
	return false
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

// offerDeferred offers the main generation C1 deferred, once: when there
// is no lease and the deployer is idle, so after any C3 return has
// finished. It goes through the deploy gate and Submit like a freshly
// confirmed generation.
func (m *Manager) offerDeferred() {
	if !m.leaseReader.Enabled() || m.leaseReader.Observe().Exists || !m.deployer.Idle() {
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
	logrus.Infof("manager: the override lease ended, deploying the deferred main generation %s", g.Uuid)
	m.deployer.Submit(g, operation)
}

// c3Candidate reports whether C3's release conditions currently hold, and
// if so, the current lease-aware testing deployment they apply to: no
// lease, nothing queued or in flight, and the current deployment is a
// lease-aware testing deployment that finished, successfully or not (a
// failed `switch-to-configuration test` usually leaves the system
// activated). It never mutates state: both checkRelease (which acts on it)
// and driftStatus (which only reports it) call this.
func (m *Manager) c3Candidate() (*protobuf.Deployment, bool) {
	if !m.leaseReader.Enabled() || m.leaseReader.Observe().Exists {
		return nil, false
	}
	if !m.deployer.Idle() {
		return nil, false
	}
	dpl := m.deployer.Deployment()
	if dpl == nil || !isTestingDeployment(dpl) || !m.leaseState.IsLeaseAware(dpl.Uuid) {
		return nil, false
	}
	if dpl.Status != store.StatusToString(store.Done) && dpl.Status != store.StatusToString(store.Failed) {
		return nil, false
	}
	return dpl, true
}

// checkRelease implements C3: once an override lease ends and nothing is
// queued or in flight, every lease-aware testing deployment of the current
// deployment's branch is released, and comin returns once to the S4
// expected generation (unless already running it). The return is a
// switch-latest request, so it is never skipped as "already deployed".
func (m *Manager) checkRelease() {
	dpl, ok := m.c3Candidate()
	if !ok {
		return
	}
	deployments := m.storage.DeploymentList()
	branch := dpl.Generation.GetSelectedBranchName()
	remote := dpl.Generation.GetSelectedRemoteName()
	for _, d := range deployments {
		if !isTestingDeployment(d) {
			continue
		}
		if d.Generation.GetSelectedBranchName() != branch || d.Generation.GetSelectedRemoteName() != remote {
			continue
		}
		if !m.leaseState.IsLeaseAware(d.Uuid) || m.leaseState.IsReleased(d.Uuid) {
			continue
		}
		if err := m.leaseState.MarkReleased(d.Uuid); err != nil {
			logrus.Errorf("manager: could not mark deployment %s released: %s", d.Uuid, err)
		}
	}

	expected := m.expectedDeployment(deployments, false)
	if expected == nil {
		return
	}
	current, err := m.executor.CurrentSystem()
	if err != nil {
		logrus.Errorf("manager: could not read the current system: %s", err)
		return
	}
	if current == expected.Generation.GetOutPath() {
		return
	}
	logrus.Infof("manager: the override of %s/%s ended, returning to %s", remote, branch, expected.Generation.GetOutPath())
	m.deployer.SubmitLatest(m.resolveSwitchLatest)
}

// poll re-evaluates everything that can change without a fetch: C3, the
// deferred tier generation, and the S4 drift episode (so `since` is
// recorded even if nobody queries the status).
func (m *Manager) poll(now time.Time) {
	m.checkRelease()
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
	c3Holds               bool
}

// decideDriftState applies S4's precedence: held (lease exists) beats none
// (already at expected, or nothing expected) beats returning (work queued,
// an in-flight deploy under maxInFlightDeployAge, or C3 about to fire)
// beats leaseless. none beats returning when both hold, e.g. a git lease
// that just ended by reboot, already at expected, before C3 has run.
func decideDriftState(in driftInputs) string {
	if in.leaseExists {
		return "held"
	}
	if in.currentEqualsExpected || !in.hasExpected {
		return "none"
	}
	inFlightCounts := in.inFlight && in.inFlightAge < maxInFlightDeployAge
	if inFlightCounts || in.queued || in.c3Holds {
		return "returning"
	}
	return "leaseless"
}

func (m *Manager) inFlightInfo(now time.Time) (inFlight bool, age time.Duration) {
	if !m.deployer.IsDeploying() {
		return false, 0
	}
	dpl := m.deployer.Deployment()
	if dpl == nil || dpl.StartedAt == nil {
		return true, 0
	}
	return true, now.Sub(dpl.StartedAt.AsTime())
}

// driftStatus computes S4's `drift` object and persists the start of the
// current drift episode ("since") the first time it is observed, clearing
// it once the state is back to none. toState calls it for `comin status`,
// and poll calls it on every tick and after every finished deployment.
func (m *Manager) driftStatus(now time.Time) *protobuf.Drift {
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

	inFlight, inFlightAge := m.inFlightInfo(now)
	_, c3Holds := m.c3Candidate()

	state := decideDriftState(driftInputs{
		leaseExists:           obs.Exists,
		hasExpected:           hasExpected,
		currentEqualsExpected: hasExpected && current == expectedOutPath,
		inFlight:              inFlight,
		inFlightAge:           inFlightAge,
		queued:                m.deployer.HasQueuedWork(),
		c3Holds:               c3Holds,
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
