package manager

import (
	"context"
	"os"
	"path/filepath"
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
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// controllableExecutor is a lease-test Executor double that lets a test set
// what /run/current-system currently resolves to.
type controllableExecutor struct {
	mu      sync.Mutex
	current string
}

func (e *controllableExecutor) set(current string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.current = current
}

func (e *controllableExecutor) get() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

func (e *controllableExecutor) ReadMachineId() (string, error) { return "", nil }
func (e *controllableExecutor) NeedToReboot(_, _ string) bool  { return false }
func (e *controllableExecutor) IsStorePathExist(string) bool   { return false }
func (e *controllableExecutor) CurrentSystem() (string, error) { return e.get(), nil }
func (e *controllableExecutor) Deploy(ctx context.Context, outPath, operation string) (bool, string, error) {
	return false, "", nil
}
// Eval derives the out path from the commit, so a fetched commit C
// deploys /nix/store/C.
func (e *controllableExecutor) Eval(ctx context.Context, repositoryPath, repositorySubdir, commitId, systemAttr, hostname string) (string, string, string, error) {
	return "/nix/store/drv-" + commitId, "/nix/store/" + commitId, "", nil
}
func (e *controllableExecutor) Build(ctx context.Context, drvPath string) error { return nil }

// leaseFixture wires a real Manager (same construction path as
// cmd/run.go and manager_test.go) for the C1/C2/C3/C5/C7 tests: a real
// store and deployer (so Submit/SubmitLatest genuinely run deployments
// through the deployer's own goroutine), a controllable executor (so tests
// can set "current"), and a lease reader/state rooted in a scratch dir.
type leaseFixture struct {
	*Manager
	exec           *controllableExecutor
	leasePath      string
	leaseStatePath string
	store          *store.Store
	drainStop      chan struct{}
}

func newLeaseFixture(t *testing.T, previousDeployment *protobuf.Deployment) *leaseFixture {
	t.Helper()
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(tmp, "state.json"), filepath.Join(tmp, "gcroots"), 10, 10)
	assert.NoError(t, err)
	leaseStatePath := filepath.Join(tmp, "lease-state.json")
	ls, err := leasestate.Load(leaseStatePath)
	assert.NoError(t, err)
	leasePath := filepath.Join(tmp, "override.json")

	exec := &controllableExecutor{}
	deployFunc := func(context.Context, string, string) (bool, string, error) { return false, "", nil }
	d := deployer.New(s, deployFunc, previousDeployment, "")
	d.Run(t.Context())
	// Manager.Run() normally drains DeploymentDoneCh (buffered, size 1);
	// without a consumer here, a second deployment's completion would
	// block the deployer's goroutine forever on that send. Tests that
	// call f.Run() themselves stop this via f.stopAutoDrain().
	drainStop := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.Context().Done():
				return
			case <-drainStop:
				return
			case dpl := <-d.DeploymentDoneCh:
				s.DeploymentInsertAndCommit(dpl)
			}
		}
	}()

	b := builder.New(s, exec, "", "", "", "host", time.Second, time.Second)
	fe := fetcher.NewFetcher(utils.NewRepositoryMock())
	bc := NewConfirmer(bk, Without, 0, "")
	bc.Start()
	dc := NewConfirmer(bk, Without, 0, "")
	dc.Start()

	m := New(s, prometheus.New(), scheduler.New(), fe, b, d, "", exec, bc, dc, bk, emptyConfigurationOperations, lease.NewReader(leasePath), ls)
	return &leaseFixture{Manager: m, exec: exec, leasePath: leasePath, leaseStatePath: leaseStatePath, store: s, drainStop: drainStop}
}

// stopAutoDrain stops the fixture's background DeploymentDoneCh drain, for
// tests that call f.Run() and need to drain it themselves instead.
func (f *leaseFixture) stopAutoDrain() {
	close(f.drainStop)
}

func (f *leaseFixture) writeLease(t *testing.T, kind string) {
	t.Helper()
	content := `{"version":1,"kind":"` + kind + `"}`
	assert.NoError(t, os.WriteFile(f.leasePath, []byte(content), 0644))
}

func (f *leaseFixture) removeLease() {
	_ = os.Remove(f.leasePath)
}

// submitAndWait submits g through the real deployer and waits for the
// deployer to finish deploying it, returning the resulting deployment.
func submitAndWait(t *testing.T, f *leaseFixture, g *protobuf.Generation, operation string) *protobuf.Deployment {
	t.Helper()
	f.deployer.Submit(g, operation)
	var dpl *protobuf.Deployment
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		dpl = f.deployer.Deployment()
		assert.NotNil(c, dpl)
		assert.Equal(c, g.SelectedCommitId, dpl.Generation.SelectedCommitId)
		assert.False(c, f.deployer.IsDeploying())
	}, 3*time.Second, 20*time.Millisecond)
	return dpl
}

func mainGeneration(commit, outPath string) *protobuf.Generation {
	return &protobuf.Generation{
		Uuid:                    "gen-" + commit,
		SelectedCommitId:        commit,
		SelectedRemoteName:      "origin",
		SelectedBranchName:      "main",
		SelectedBranchIsTesting: wrapperspb.Bool(false),
		MainCommitId:            commit,
		OutPath:                 outPath,
	}
}

func testingGeneration(commit, mainCommit string) *protobuf.Generation {
	return &protobuf.Generation{
		Uuid:                    "gen-" + commit,
		SelectedCommitId:        commit,
		SelectedRemoteName:      "origin",
		SelectedBranchName:      "testing-r1",
		SelectedBranchIsTesting: wrapperspb.Bool(true),
		MainCommitId:            mainCommit,
		OutPath:                 "/nix/store/" + commit,
	}
}

func marshalStatus(t *testing.T, s *protobuf.State) string {
	t.Helper()
	marshaler := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	buf, err := marshaler.Marshal(s)
	assert.NoError(t, err)
	return string(buf)
}

// ---------------------------------------------------------------------
// C1: Freeze
// ---------------------------------------------------------------------

func TestC1FreezeBlocksMainDeploymentWhileLeaseHeld(t *testing.T) {
	f := newLeaseFixture(t, nil)
	f.writeLease(t, "git")
	_, ok := f.leaseDeployDecision(mainGeneration("m2", "/nix/store/m2"))
	assert.False(t, ok, "a main generation must not deploy while a lease is held")
}

func TestC1AllowsMainDeploymentWithoutLease(t *testing.T) {
	f := newLeaseFixture(t, nil)
	_, ok := f.leaseDeployDecision(mainGeneration("m2", "/nix/store/m2"))
	assert.True(t, ok)
}

func TestC1DisabledLeavesDecisionUnchanged(t *testing.T) {
	// overrideLeaseFile null: the lease reader is disabled, so even a
	// lease file materializing at that (unconfigured) path changes
	// nothing (Preserve).
	f := newLeaseFixture(t, nil)
	f.Manager.leaseReader = lease.NewReader("")
	f.writeLease(t, "git")
	_, ok := f.leaseDeployDecision(mainGeneration("m2", "/nix/store/m2"))
	assert.True(t, ok)
}

// ---------------------------------------------------------------------
// C2: Kind gate
// ---------------------------------------------------------------------

func TestC2SessionAndClosureBlockTestingHead(t *testing.T) {
	for _, kind := range []string{"session", "closure"} {
		f := newLeaseFixture(t, nil)
		f.writeLease(t, kind)
		_, ok := f.leaseDeployDecision(testingGeneration("t1", ""))
		assert.False(t, ok, "kind %s must block a new testing head", kind)
	}
}

func TestC2GitOrNoLeaseDeploysDescendantTestingHead(t *testing.T) {
	f := newLeaseFixture(t, nil)
	// No held main yet: the descent check is a no-op.
	op, ok := f.leaseDeployDecision(testingGeneration("t1", "m1"))
	assert.True(t, ok)
	assert.Equal(t, "test", op)

	f.writeLease(t, "git")
	op, ok = f.leaseDeployDecision(testingGeneration("t2", "m1"))
	assert.True(t, ok)
	assert.Equal(t, "test", op)
}

// ---------------------------------------------------------------------
// C3: Release
// ---------------------------------------------------------------------

// distinctSwitchDeploymentsOf counts distinct deployment UUIDs in the store
// deploying commit with operation "switch" (store.DeploymentList can
// contain duplicate rows for the same UUID - a pre-existing, out-of-scope
// store.go quirk - so this dedupes by UUID).
func distinctSwitchDeploymentsOf(f *leaseFixture, commit string) int {
	seen := map[string]bool{}
	for _, d := range f.store.DeploymentList() {
		if d.Operation == "switch" && d.Generation.GetSelectedCommitId() == commit {
			seen[d.Uuid] = true
		}
	}
	return len(seen)
}

func TestC3ReleasesAllLeaseAwareTestingDeploysAndReturnsOnceToM(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	seeded := distinctSwitchDeploymentsOf(f, "m1")
	f.writeLease(t, "git")

	x1 := submitAndWait(t, f, testingGeneration("t1", "m1"), "test")
	assert.NoError(t, f.leaseState.MarkLeaseAware(x1.Uuid))
	// Extend: a second testing deployment on the same branch.
	x2 := submitAndWait(t, f, testingGeneration("t2", "m1"), "test")
	assert.NoError(t, f.leaseState.MarkLeaseAware(x2.Uuid))

	// A legacy deployment on the same branch, made by pre-round comin:
	// no lease-aware mark. Mutant (d): release it anyway.
	legacy := &protobuf.Deployment{
		Uuid:       "legacy-t0",
		Operation:  "test",
		Status:     store.StatusToString(store.Done),
		Generation: testingGeneration("t0", "m1"),
	}
	f.store.DeploymentInsert(legacy)

	f.removeLease() // ended by operator: current system is still X2's, not M's.
	f.checkRelease()

	assert.True(t, f.leaseState.IsReleased(x1.Uuid), "the older extend deployment must be released too")
	assert.True(t, f.leaseState.IsReleased(x2.Uuid))
	assert.False(t, f.leaseState.IsReleased(legacy.Uuid), "a pre-round deployment with no lease-aware mark must never be released")

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "m1", f.deployer.Deployment().Generation.GetSelectedCommitId())
		assert.Equal(c, "switch", f.deployer.Deployment().Operation)
	}, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, seeded+1, distinctSwitchDeploymentsOf(f, "m1"), "exactly one return deployment")

	// A second checkRelease (e.g. the next poll tick) must not return
	// again: the current deployment is now M itself, not a testing one.
	f.checkRelease()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, seeded+1, distinctSwitchDeploymentsOf(f, "m1"))
	// A new head of B still deploys.
	_, ok := f.leaseDeployDecision(testingGeneration("t3", "m1"))
	assert.True(t, ok)
}

// Mutant (a): drop the released check in expectedDeployment. Isolated from
// TestC3Releases...: here nothing more recent than the released testing
// deployment exists, so only the IsReleased filter can make M win.
func TestC3ExpectedDeploymentSkipsReleasedTestingHeads(t *testing.T) {
	f := newLeaseFixture(t, nil)
	mDpl := &protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done),
		Generation: mainGeneration("m1", "/nix/store/m1"),
	}
	f.store.DeploymentInsert(mDpl)
	xDpl := &protobuf.Deployment{
		Uuid: "t1-dpl", Operation: "test", Status: store.StatusToString(store.Done),
		Generation: testingGeneration("t1", "m1"),
	}
	// Inserted after M: more recent in DeploymentList order.
	f.store.DeploymentInsert(xDpl)
	assert.NoError(t, f.leaseState.MarkLeaseAware(xDpl.Uuid))
	assert.NoError(t, f.leaseState.MarkReleased(xDpl.Uuid))

	got := f.expectedDeployment(f.store.DeploymentList(), true) // git lease live: testing counts
	assert.NotNil(t, got)
	assert.Equal(t, "m1", got.Generation.GetSelectedCommitId(), "a released testing deployment must never be expected again, even while testing counts")
}

func TestC3RebootEndedLeaseReturnsNothingAndTargetsMForSwitchLatest(t *testing.T) {
	for _, n := range []int{1, 2} {
		f := newLeaseFixture(t, nil)
		m := mainGeneration("m1", "/nix/store/m1")
		f.store.DeploymentInsert(&protobuf.Deployment{
			Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
		})
		seeded := distinctSwitchDeploymentsOf(f, "m1")
		f.writeLease(t, "git")

		var last *protobuf.Deployment
		for i := range n {
			last = submitAndWait(t, f, testingGeneration("t"+string(rune('1'+i)), "m1"), "test")
			assert.NoError(t, f.leaseState.MarkLeaseAware(last.Uuid))
		}

		// Reboot: the lease (never persisted) is gone, and
		// /run/current-system is back to M.
		f.removeLease()
		f.exec.set("/nix/store/m1")

		f.checkRelease()
		time.Sleep(150 * time.Millisecond)
		assert.Equal(t, seeded, distinctSwitchDeploymentsOf(f, "m1"), "n=%d: reboot already returned the machine, comin must not deploy again", n)

		drift := f.driftStatus(time.Now())
		assert.Equal(t, "none", drift.State)

		gen, err := f.resolveExpectedGeneration()
		assert.NoError(t, err)
		assert.Equal(t, "m1", gen.SelectedCommitId)
	}
}

// Mutant (f): evaluate only on fetch events. The lease is still present
// when Run starts (so the one-time startup checkRelease call finds
// nothing to do); only removed afterwards, so only the periodic ticker -
// not a fetch event, not the startup call - can catch it.
func TestC3NoFetchEventsReturnsWithinOnePollPeriod(t *testing.T) {
	x1 := &protobuf.Deployment{
		Uuid:       "t1-dpl",
		Operation:  "test",
		Status:     store.StatusToString(store.Done),
		Generation: testingGeneration("t1", "m1"),
	}
	f := newLeaseFixture(t, x1)
	f.stopAutoDrain()
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	f.store.DeploymentInsert(x1)
	assert.NoError(t, f.leaseState.MarkLeaseAware(x1.Uuid))
	f.writeLease(t, "git")

	f.SetPollPeriod(30 * time.Millisecond)
	go f.Run(t.Context())
	// Let the startup call and at least one tick pass with the lease
	// still present, proving neither alone triggers the release.
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, "t1", f.deployer.Deployment().Generation.GetSelectedCommitId())

	f.removeLease()
	// No fetch events, no manual checkRelease call: only a later tick
	// of Run's ticker can catch this.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "m1", f.deployer.Deployment().Generation.GetSelectedCommitId())
	}, 2*time.Second, 20*time.Millisecond)
}

// Mutant (c): release while a deployment is in flight. A gated
// postDeploymentCommand keeps IsDeploying() true after the deployment's
// Status has already flipped to Done (deployer.go sets Status=Done, then
// runs the post-deployment command, then clears isDeploying) - the exact
// window C3's "no deployment in flight" guard must cover.
func TestC3DoesNotReleaseWhileDeploymentInFlight(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "release-post-deploy")
	script := filepath.Join(tmp, "post-deploy.sh")
	assert.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nwhile [ ! -f \""+marker+"\" ]; do sleep 0.01; done\n"), 0755))

	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(tmp, "state.json"), filepath.Join(tmp, "gcroots"), 10, 10)
	assert.NoError(t, err)
	ls, err := leasestate.Load(filepath.Join(tmp, "lease-state.json"))
	assert.NoError(t, err)
	leasePath := filepath.Join(tmp, "override.json")
	exec := &controllableExecutor{}
	deployFunc := func(context.Context, string, string) (bool, string, error) { return false, "", nil }
	d := deployer.New(s, deployFunc, nil, script)
	d.Run(t.Context())
	go func() {
		for dpl := range d.DeploymentDoneCh {
			s.DeploymentInsertAndCommit(dpl)
		}
	}()

	m := &Manager{
		storage:                 s,
		deployer:                d,
		executor:                exec,
		configurationOperations: emptyConfigurationOperations,
		leaseReader:             lease.NewReader(leasePath),
		leaseState:              ls,
	}
	mg := mainGeneration("m1", "/nix/store/m1")
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mg})
	assert.NoError(t, os.WriteFile(leasePath, []byte(`{"version":1,"kind":"git"}`), 0644))

	d.Submit(testingGeneration("t1", "m1"), "test")
	// The deploy function returns instantly, so the deployment reaches
	// Status=Done almost immediately, but the post-deployment command is
	// blocked on the marker file: isDeploying stays true throughout.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		dpl := d.Deployment()
		assert.NotNil(c, dpl)
		assert.Equal(c, store.StatusToString(store.Done), dpl.Status)
		assert.True(c, d.IsDeploying())
	}, 3*time.Second, 10*time.Millisecond)
	x1 := d.Deployment()
	assert.NoError(t, ls.MarkLeaseAware(x1.Uuid))

	os.Remove(leasePath)
	m.checkRelease()
	assert.False(t, ls.IsReleased(x1.Uuid), "must not release while the post-deployment command is still running")

	// Unblock the post-deployment command: only now is nothing in flight.
	assert.NoError(t, os.WriteFile(marker, []byte("go"), 0644))
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.IsDeploying())
	}, 3*time.Second, 10*time.Millisecond)
	m.checkRelease()
	assert.True(t, ls.IsReleased(x1.Uuid))
}

// C3 amendment (N5/g): a deployment is testing iff
// Generation.SelectedBranchIsTesting, not store.IsTesting (operation-keyed).
// A persist-triggered switch-latest redeploy of a testing head (operation
// "switch") must still be released and trigger the return. Mutant:
// classify testing with store.IsTesting.
func TestC3ClassifiesTestingBySelectedBranchNotOperation(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	seeded := distinctSwitchDeploymentsOf(f, "m1")
	f.writeLease(t, "git")

	x := submitAndWait(t, f, testingGeneration("t1", "m1"), "test")
	assert.NoError(t, f.leaseState.MarkLeaseAware(x.Uuid))

	// R3's persist flow: switch-latest redeploys the testing head with
	// operation "switch" (X').
	f.deployer.SubmitLatest(func() (*protobuf.Generation, error) { return x.Generation, nil })
	var xPrime *protobuf.Deployment
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		xPrime = f.deployer.Deployment()
		assert.Equal(c, "switch", xPrime.Operation)
		assert.False(c, f.deployer.IsDeploying())
	}, 3*time.Second, 20*time.Millisecond)
	assert.True(t, isTestingDeployment(xPrime), "X' still descends from a testing generation")
	assert.NoError(t, f.leaseState.MarkLeaseAware(xPrime.Uuid))

	f.removeLease()
	f.checkRelease()

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "m1", f.deployer.Deployment().Generation.GetSelectedCommitId())
		assert.Equal(c, "switch", f.deployer.Deployment().Operation)
	}, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, seeded+1, distinctSwitchDeploymentsOf(f, "m1"))
}

// ---------------------------------------------------------------------
// C5: S4 drift status
// ---------------------------------------------------------------------

func TestDecideDriftStatePrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   driftInputs
		want string
	}{
		{"held wins over everything", driftInputs{leaseExists: true, currentEqualsExpected: false, queued: true, inFlight: true, c3Holds: true}, "held"},
		{"none when current equals expected", driftInputs{currentEqualsExpected: true}, "none"},
		{"none when nothing expected yet", driftInputs{hasExpected: false, currentEqualsExpected: false}, "none"},
		{"none beats returning when both hold", driftInputs{hasExpected: true, currentEqualsExpected: true, queued: true, c3Holds: true, inFlight: true}, "none"},
		{"returning when work is queued", driftInputs{hasExpected: true, currentEqualsExpected: false, queued: true}, "returning"},
		{"returning when a fresh deploy is in flight", driftInputs{hasExpected: true, currentEqualsExpected: false, inFlight: true, inFlightAge: time.Minute}, "returning"},
		{"returning when C3 is about to fire", driftInputs{hasExpected: true, currentEqualsExpected: false, c3Holds: true}, "returning"},
		{"leaseless when nothing queued or in flight", driftInputs{hasExpected: true, currentEqualsExpected: false}, "leaseless"},
		{"leaseless when the in-flight deploy is over the age bound", driftInputs{hasExpected: true, currentEqualsExpected: false, inFlight: true, inFlightAge: 45 * time.Minute}, "leaseless"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, decideDriftState(c.in))
		})
	}
}

// Mutant: no age bound on an in-flight deploy (switch-to-configuration has
// no timeout; a hung activation must eventually read leaseless, not
// returning forever).
func TestDecideDriftStateInFlightAgeBoundIsExclusive(t *testing.T) {
	justUnder := driftInputs{hasExpected: true, currentEqualsExpected: false, inFlight: true, inFlightAge: maxInFlightDeployAge - time.Second}
	assert.Equal(t, "returning", decideDriftState(justUnder))
	atOrOver := driftInputs{hasExpected: true, currentEqualsExpected: false, inFlight: true, inFlightAge: maxInFlightDeployAge}
	assert.Equal(t, "leaseless", decideDriftState(atOrOver))
}

func TestDriftStatusSinceIsPersistedAcrossReload(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	// current != expected, no lease, nothing queued: leaseless drift.
	now := time.Now().UTC()
	d1 := f.driftStatus(now)
	assert.Equal(t, "leaseless", d1.State)
	assert.NotNil(t, d1.Since)

	// Simulate a comin restart: reload leasestate from the same file.
	reloaded, err := leasestate.Load(f.leaseStatePath)
	assert.NoError(t, err)
	f.Manager.leaseState = reloaded

	d2 := f.driftStatus(now.Add(5 * time.Minute))
	assert.Equal(t, "leaseless", d2.State)
	assert.NotNil(t, d2.Since)
	assert.Equal(t, d1.Since.AsTime(), d2.Since.AsTime(), "since must survive a restart, not reset to the new observation time")
}

// C5 fixture: the lease file was just removed and C3 has not yet
// evaluated (the current deployment is still the lease-aware testing one)
// => returning, not leaseless.
func TestC5ReturningWhenLeaseJustRemovedBeforeC3Runs(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	f.writeLease(t, "git")
	x := submitAndWait(t, f, testingGeneration("t1", "m1"), "test")
	assert.NoError(t, f.leaseState.MarkLeaseAware(x.Uuid))

	f.removeLease() // C3 has not run yet: checkRelease() was never called.
	d := f.driftStatus(time.Now())
	assert.Equal(t, "returning", d.State)
}

// C5 fixture: a tier (main) deploy mid-activation, current-system already
// repointed to the new out path but the deployment not yet Done, is
// "returning" (not "none", even though current already equals the
// in-flight deployment's target, because "expected" still points at the
// prior done deployment until this one finishes).
func TestC5ReturningDuringTierDeployMidActivation(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(tmp, "state.json"), filepath.Join(tmp, "gcroots"), 10, 10)
	assert.NoError(t, err)
	ls, err := leasestate.Load(filepath.Join(tmp, "lease-state.json"))
	assert.NoError(t, err)
	exec := &controllableExecutor{}
	deployDone := make(chan struct{})
	// switch-to-configuration repoints /run/current-system as one of
	// its first actions, well before the deploy function returns and
	// the deployment's Status flips to Done - this deployFunc models
	// that ordering directly instead of relying on deployer.go's
	// Status-before-post-deploy-command sequencing.
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		exec.set("/nix/store/m2")
		<-deployDone
		return false, "", nil
	}
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())
	go func() {
		for dpl := range d.DeploymentDoneCh {
			s.DeploymentInsertAndCommit(dpl)
		}
	}()

	m := &Manager{
		storage:                 s,
		deployer:                d,
		executor:                exec,
		configurationOperations: emptyConfigurationOperations,
		leaseReader:             lease.NewReader(""), // no override-lease feature: S4 is always on regardless
		leaseState:              ls,
	}
	mOld := mainGeneration("m1", "/nix/store/m1")
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mOld})

	mNew := mainGeneration("m2", "/nix/store/m2")
	d.Submit(mNew, "switch")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "/nix/store/m2", exec.get())
	}, 3*time.Second, 10*time.Millisecond)
	// The deployment is still in flight (Status not Done yet): expected
	// still points at the prior done deployment (M1), so current != expected.
	assert.True(t, d.IsDeploying())

	drift := m.driftStatus(time.Now())
	assert.Equal(t, "returning", drift.State)

	close(deployDone)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, d.IsDeploying())
	}, 3*time.Second, 10*time.Millisecond)
}

// C5: S4 exactly as contracted, asserted on the actual `comin status
// --json` bytes (protojson, UseProtoNames, EmitUnpopulated).
func TestC5StatusJsonBytesContract(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	f.exec.set("/nix/store/m1")

	body := marshalStatus(t, f.toState())
	assert.Contains(t, body, `"drift":`)
	assert.Contains(t, body, `"expected_out_path":"/nix/store/m1"`)
	assert.Contains(t, body, `"state":"none"`)
	assert.NotContains(t, body, `"drift":null`)
}

// ---------------------------------------------------------------------
// C7: switch-latest
// ---------------------------------------------------------------------

func TestC7SwitchLatestTargetsExpectedNotLatestStoreEntry_RebootReleased(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	// The latest store entry is a testing deployment released by a
	// reboot (older than M in append order, but store.DeploymentList
	// puts the most recently INSERTED row first; here it is the "latest"
	// entry in the sense GetDeploymentLastest would have picked).
	x := &protobuf.Deployment{
		Uuid:       "t1-dpl",
		Operation:  "test",
		Status:     store.StatusToString(store.Done),
		Generation: testingGeneration("t1", "m1"),
		EndedAt:    timestamppb.New(time.Now()),
	}
	f.store.DeploymentInsert(x)
	assert.NoError(t, f.leaseState.MarkLeaseAware(x.Uuid))
	assert.NoError(t, f.leaseState.MarkReleased(x.Uuid))

	gen, err := f.resolveExpectedGeneration()
	assert.NoError(t, err)
	assert.Equal(t, "m1", gen.SelectedCommitId, "a released testing deployment must never be the switch-latest target")
}

func TestC7SwitchLatestTargetsExpectedNotLatestStoreEntry_Failed(t *testing.T) {
	f := newLeaseFixture(t, nil)
	m := mainGeneration("m1", "/nix/store/m1")
	f.store.DeploymentInsert(&protobuf.Deployment{
		Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m,
	})
	failed := &protobuf.Deployment{
		Uuid:       "m2-dpl",
		Operation:  "switch",
		Status:     store.StatusToString(store.Failed),
		Generation: mainGeneration("m2", "/nix/store/m2"),
		EndedAt:    timestamppb.New(time.Now()),
	}
	f.store.DeploymentInsert(failed)

	gen, err := f.resolveExpectedGeneration()
	assert.NoError(t, err)
	assert.Equal(t, "m1", gen.SelectedCommitId, "a failed deployment must never be the switch-latest target")
}

// A git lease is live and a newer testing deploy is in flight when persist
// (switch-latest) is requested: the newer one is switched, because
// SubmitLatest resolves at deploy time, after the in-flight one finishes.
func TestC7SwitchLatestTargetsNewerTestingDeployStillInFlight(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "marker")
	script := filepath.Join(tmp, "post-deploy.sh")
	assert.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nwhile [ ! -f \""+marker+"\" ]; do sleep 0.01; done\n"), 0755))

	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(tmp, "state.json"), filepath.Join(tmp, "gcroots"), 10, 10)
	assert.NoError(t, err)
	ls, err := leasestate.Load(filepath.Join(tmp, "lease-state.json"))
	assert.NoError(t, err)
	leasePath := filepath.Join(tmp, "override.json")
	exec := &controllableExecutor{}
	deployFunc := func(context.Context, string, string) (bool, string, error) { return false, "", nil }
	d := deployer.New(s, deployFunc, nil, script)
	d.Run(t.Context())
	go func() {
		for dpl := range d.DeploymentDoneCh {
			s.DeploymentInsertAndCommit(dpl)
		}
	}()

	m := &Manager{
		storage:                 s,
		deployer:                d,
		executor:                exec,
		configurationOperations: emptyConfigurationOperations,
		leaseReader:             lease.NewReader(leasePath),
		leaseState:              ls,
	}
	mg := mainGeneration("m1", "/nix/store/m1")
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: mg})
	assert.NoError(t, os.WriteFile(leasePath, []byte(`{"version":1,"kind":"git"}`), 0644))

	// The newer testing deploy is in flight (Status=Done, but the
	// post-deployment command is still blocked).
	d.Submit(testingGeneration("t2", "m1"), "test")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		dpl := d.Deployment()
		assert.NotNil(c, dpl)
		assert.Equal(c, store.StatusToString(store.Done), dpl.Status)
	}, 3*time.Second, 10*time.Millisecond)
	assert.NoError(t, ls.MarkLeaseAware(d.Deployment().Uuid))

	assert.NoError(t, m.SwitchDeploymentLatest())
	assert.True(t, d.HasQueuedWork(), "the switch-latest request is queued, waiting for the in-flight deploy to actually finish")

	assert.NoError(t, os.WriteFile(marker, []byte("go"), 0644))
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "t2", d.Deployment().Generation.GetSelectedCommitId())
		assert.Equal(c, "switch", d.Deployment().Operation)
	}, 3*time.Second, 10*time.Millisecond)
}

// C7: a switch-latest request survives a comin restart between the
// request and the deployer picking it up.
func TestC7SwitchLatestSurvivesRestart(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, filepath.Join(tmp, "state.json"), filepath.Join(tmp, "gcroots"), 10, 10)
	assert.NoError(t, err)
	leaseStatePath := filepath.Join(tmp, "lease-state.json")
	ls, err := leasestate.Load(leaseStatePath)
	assert.NoError(t, err)
	m1 := mainGeneration("m1", "/nix/store/m1")
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "m1-dpl", Operation: "switch", Status: store.StatusToString(store.Done), Generation: m1})

	exec1 := &controllableExecutor{}
	deployFunc := func(context.Context, string, string) (bool, string, error) { return false, "", nil }
	d1 := deployer.New(s, deployFunc, nil, "")
	// Note: d1.Run is never called, simulating the request being made
	// and comin restarting before the deployer goroutine (re)starts.
	mgr1 := &Manager{
		storage:                 s,
		deployer:                d1,
		executor:                exec1,
		configurationOperations: emptyConfigurationOperations,
		leaseReader:             lease.NewReader(""),
		leaseState:              ls,
	}
	assert.NoError(t, mgr1.SwitchDeploymentLatest())
	assert.True(t, ls.PendingSwitchLatest())

	// Restart: fresh deployer and leasestate reload from the same files.
	// d2.Run is started by mgr2.Run itself, and mgr2.Run also drains
	// DeploymentDoneCh (which is what actually clears PendingSwitchLatest).
	ls2, err := leasestate.Load(leaseStatePath)
	assert.NoError(t, err)
	assert.True(t, ls2.PendingSwitchLatest())
	d2 := deployer.New(s, deployFunc, nil, "")
	mgr2 := New(s, prometheus.New(), scheduler.New(), fetcher.NewFetcher(utils.NewRepositoryMock()),
		builder.New(s, exec1, "", "", "", "host", time.Second, time.Second), d2, "", exec1,
		NewConfirmer(bk, Without, 0, ""), NewConfirmer(bk, Without, 0, ""), bk, emptyConfigurationOperations,
		lease.NewReader(""), ls2)

	go mgr2.Run(t.Context())
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "m1", d2.Deployment().Generation.GetSelectedCommitId())
		assert.Equal(c, "switch", d2.Deployment().Operation)
	}, 3*time.Second, 20*time.Millisecond)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.False(c, ls2.PendingSwitchLatest())
	}, 3*time.Second, 20*time.Millisecond)
}
