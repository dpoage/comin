package manager

// This file holds the manager's override-lease-aware decisions: the tier
// freeze and kind gate applied before submitting a generation to the
// deployer (C1, C2), releasing a testing deployment once its override ends
// and returning to the expected generation (C3), and the S4 drift status
// (C5) that shares its "expected deployment" query and "would C3 fire"
// predicate with the release logic so the two can never disagree.

import (
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
// is present (C1), cannot advance any further. It picks by EndedAt rather
// than trusting DeploymentList's order: a deployment's Status flips to
// Done, and a resolver can run against it, before DeploymentInsertAndCommit
// (called from the manager's own DeploymentDoneCh handler, a separate
// goroutine) has re-sorted it to the front of the list.
func (m *Manager) heldMainCommitId() string {
	latest := latestDeployment(m.storage.DeploymentList(), func(d *protobuf.Deployment) bool {
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
func (m *Manager) expectedDeployment(countTesting bool) *protobuf.Deployment {
	return latestDeployment(m.storage.DeploymentList(), func(d *protobuf.Deployment) bool {
		if m.leaseState.IsReleased(d.Uuid) {
			return false
		}
		return countTesting || !isTestingDeployment(d)
	})
}

// latestDeployment returns the Done deployment satisfying keep with the
// most recent EndedAt.
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

// leaseDeployDecision applies C1 (freeze) and C2 (kind gate) to a
// generation about to be submitted for deployment. When the lease reader is
// disabled (overrideLeaseFile is null), this always returns ok=true: comin
// behaves exactly as it did before this feature existed.
func (m *Manager) leaseDeployDecision(g *protobuf.Generation) (operation string, ok bool) {
	operation = m.getOperationFromConfigurationOperations(g.SelectedRemoteName, g.SelectedBranchName)
	if !m.leaseReader.Enabled() {
		return operation, true
	}
	obs := m.leaseReader.Observe()

	if !isTestingGeneration(g) {
		// C1: while a lease exists, comin starts no deployment of a
		// main generation, advanced or held, except an explicit
		// switch-latest (which never goes through this path).
		if obs.Exists {
			logrus.Infof("manager: skipping deployment of the main generation %s: an override lease is held", g.Uuid)
			return operation, false
		}
		return operation, true
	}

	// C2: kind gate. A lease of kind session or closure blocks a new
	// testing head; kind git (or no lease) requires it to descend from
	// the held main commit.
	if obs.Exists && !obs.IsGit() {
		logrus.Infof("manager: skipping deployment of the testing generation %s: override lease kind %q is not git", g.Uuid, obs.Kind)
		return operation, false
	}
	if held := m.heldMainCommitId(); held != "" && g.GetMainCommitId() != held {
		logrus.Infof("manager: skipping deployment of the testing generation %s: it does not descend from the held main commit %s", g.Uuid, held)
		return operation, false
	}
	return operation, true
}

// c3Candidate reports whether C3's release conditions currently hold, and
// if so, the current lease-aware testing deployment they apply to. It never
// mutates state: both checkRelease (which acts on it) and driftStatus
// (which only reports it) call this so the two can never disagree.
func (m *Manager) c3Candidate() (*protobuf.Deployment, bool) {
	if !m.leaseReader.Enabled() {
		return nil, false
	}
	if m.leaseReader.Observe().Exists {
		return nil, false
	}
	if m.deployer.IsDeploying() {
		return nil, false
	}
	dpl := m.deployer.Deployment()
	if dpl == nil || dpl.Status != store.StatusToString(store.Done) {
		return nil, false
	}
	if !isTestingDeployment(dpl) {
		return nil, false
	}
	if !m.leaseState.IsLeaseAware(dpl.Uuid) {
		return nil, false
	}
	return dpl, true
}

// checkRelease implements C3: once an override lease ends and no deployment
// is in flight, every lease-aware testing deployment of the current
// deployment's branch is released, and comin returns once to the S4
// expected generation (unless already running it).
func (m *Manager) checkRelease() {
	dpl, ok := m.c3Candidate()
	if !ok {
		return
	}
	branch := dpl.Generation.GetSelectedBranchName()
	remote := dpl.Generation.GetSelectedRemoteName()
	for _, d := range m.storage.DeploymentList() {
		if !isTestingDeployment(d) {
			continue
		}
		if d.Generation.GetSelectedBranchName() != branch || d.Generation.GetSelectedRemoteName() != remote {
			continue
		}
		if !m.leaseState.IsLeaseAware(d.Uuid) {
			continue
		}
		if m.leaseState.IsReleased(d.Uuid) {
			continue
		}
		if err := m.leaseState.MarkReleased(d.Uuid); err != nil {
			logrus.Errorf("manager: could not mark deployment %s released: %s", d.Uuid, err)
		}
	}

	expected := m.expectedDeployment(false)
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
	m.deployer.Submit(expected.Generation, "switch")
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
// it once the state is back to none. It is called both by toState() (so
// `comin status --json` always reflects it) and by the poll ticker (so
// "since" stays accurate even if nothing ever queries status).
func (m *Manager) driftStatus(now time.Time) *protobuf.Drift {
	obs := lease.Observation{}
	if m.leaseReader.Enabled() {
		obs = m.leaseReader.Observe()
	}
	expected := m.expectedDeployment(obs.IsGit())
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
