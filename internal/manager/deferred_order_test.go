package manager

import (
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// evalAt is a copy of g evaluated sec seconds after the epoch under a new uuid.
func evalAt(g *protobuf.Generation, uuid string, sec int64) *protobuf.Generation {
	c := proto.CloneOf(g)
	c.Uuid = uuid
	c.EvalStartedAt = timestamppb.New(time.Unix(sec, 0))
	return c
}

func (r *rig) forceDeferred(g *protobuf.Generation) {
	r.m.deferredMu.Lock()
	defer r.m.deferredMu.Unlock()
	r.m.deferred = g
}

func (r *rig) deferredUuid() string {
	g := r.m.peekDeferred()
	if g == nil {
		return ""
	}
	return g.Uuid
}

// deployMain deploys a main generation of commit and waits for the deploy log
// to end with it.
func (r *rig) deployMain(t *testing.T, commit string) {
	t.Helper()
	want := append(r.deployLog(), commit+"/switch")
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration(commit, "/nix/store/"+commit)))
	r.waitDeploys(t, want...)
}

// quiesce returns once the manager loop has finished the iteration it was in.
func (r *rig) quiesce() { _ = r.m.GetState() }

type gateCase struct {
	name     string
	setup    func(t *testing.T, r *rig) *protobuf.Generation
	admitted bool
	// clears: the decision empties the deferred slot; otherwise it defers g.
	clears bool
}

var gateCases = []gateCase{
	{name: "lease held", setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.deployMain(t, "m1")
		r.stop()
		r.writeLease(t, "session")
		return mainGeneration("m2", "/nix/store/m2")
	}},
	{name: "ended override not released", setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.overrideOn(t, 1)
		r.stop()
		require.NoError(t, os.Remove(r.lease))
		return mainGeneration("m2", "/nix/store/m2")
	}},
	{name: "foreign running system", setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.deployMain(t, "m1")
		r.stop()
		r.exec.set("/nix/store/foreign")
		return mainGeneration("m2", "/nix/store/m2")
	}},
	{name: "rolled back from", clears: true, setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.deployMain(t, "m1")
		r.deployMain(t, "m2")
		r.stop()
		r.exec.set("/nix/store/m1")
		return mainGeneration("m3", "/nix/store/m2")
	}},
	{name: "already running", clears: true, setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.deployMain(t, "m1")
		r.stop()
		return mainGeneration("m1", "/nix/store/m1")
	}},
	{name: "admitted", clears: true, admitted: true, setup: func(t *testing.T, r *rig) *protobuf.Generation {
		r.deployMain(t, "m1")
		r.stop()
		return mainGeneration("m2", "/nix/store/m2")
	}},
}

func newGateRig(t *testing.T) *rig {
	r := newRig(t, t.TempDir())
	r.m.SetPollPeriod(time.Hour)
	r.start(t)
	return r
}

// A decision on g never replaces or clears a deferred generation evaluated
// after g, on every branch of the gate and through both of its callers.
func TestDeferredGateBranchesKeepNewerGeneration(t *testing.T) {
	entries := map[string]func(r *rig, g *protobuf.Generation) bool{
		"confirmed": func(r *rig, g *protobuf.Generation) bool {
			_, ok := r.m.leaseDeployDecision(g)
			return ok
		},
		"admitQueued": func(r *rig, g *protobuf.Generation) bool { return r.m.admitQueued(g) },
	}
	for _, c := range gateCases {
		for entry, decide := range entries {
			t.Run(c.name+"/"+entry, func(t *testing.T) {
				t.Run("newer deferred survives", func(t *testing.T) {
					r := newGateRig(t)
					g := evalAt(c.setup(t, r), "gen-decided", 100)
					r.forceDeferred(evalAt(mainGeneration("m9", "/nix/store/m9"), "gen-newer", 200))
					assert.Equal(t, c.admitted, decide(r, g))
					assert.Equal(t, "gen-newer", r.deferredUuid())
				})
				t.Run("older deferred is replaced", func(t *testing.T) {
					r := newGateRig(t)
					g := evalAt(c.setup(t, r), "gen-decided", 100)
					r.forceDeferred(evalAt(mainGeneration("m0", "/nix/store/m0"), "gen-older", 50))
					assert.Equal(t, c.admitted, decide(r, g))
					if c.clears {
						assert.Equal(t, "", r.deferredUuid())
					} else {
						assert.Equal(t, "gen-decided", r.deferredUuid())
					}
				})
			})
		}
	}
}

// The ordering is strict on EvalStartedAt, and a missing one reads as the zero time.
func TestDeferredOrderBoundaries(t *testing.T) {
	at := func(sec int64) *int64 { return &sec }
	for _, c := range []struct {
		name     string
		deferred *int64
		decided  *int64
		kept     bool
	}{
		{"deferred later", at(200), at(100), true},
		{"equal", at(100), at(100), false},
		{"both nil", nil, nil, false},
		{"deferred earlier", at(50), at(100), false},
		{"deferred nil, decided stamped", nil, at(100), false},
		{"deferred stamped, decided nil", at(100), nil, true},
	} {
		for _, gc := range []gateCase{gateCases[0], gateCases[5]} {
			t.Run(c.name+"/"+gc.name, func(t *testing.T) {
				r := newRig(t, t.TempDir())
				r.m.SetPollPeriod(time.Hour)
				if gc.name == "lease held" {
					r.writeLease(t, "session")
				}
				g := mainGeneration("m2", "/nix/store/m2")
				d := mainGeneration("m9", "/nix/store/m9")
				if c.decided != nil {
					g = evalAt(g, g.Uuid, *c.decided)
				}
				if c.deferred != nil {
					d = evalAt(d, d.Uuid, *c.deferred)
				}
				r.forceDeferred(d)
				_, ok := r.m.leaseDeployDecision(g)
				assert.Equal(t, gc.admitted, ok)
				switch {
				case c.kept:
					assert.Equal(t, d.Uuid, r.deferredUuid())
				case gc.clears:
					assert.Equal(t, "", r.deferredUuid())
				default:
					assert.Equal(t, g.Uuid, r.deferredUuid())
				}
			})
		}
	}
}

// hookExecutor runs onCurrent before each read of the running system.
type hookExecutor struct {
	*controllableExecutor
	onCurrent func()
}

func (e *hookExecutor) CurrentSystem() (string, error) {
	e.onCurrent()
	return e.controllableExecutor.CurrentSystem()
}

// offerDeferred takes the deferred generation before it decides on it, so a
// newer generation can only arrive in between: the hook installs one at the
// first read of the running system after the take.
func TestDeferredOfferKeepsNewerGeneration(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, r *rig) *protobuf.Generation
		flip    bool
		deploys bool
	}{
		{name: "admitted", deploys: true, setup: func(t *testing.T, r *rig) *protobuf.Generation {
			r.deployMain(t, "m1")
			return mainGeneration("m2", "/nix/store/m2")
		}},
		{name: "already running", setup: func(t *testing.T, r *rig) *protobuf.Generation {
			r.deployMain(t, "m1")
			return mainGeneration("m1", "/nix/store/m1")
		}},
		{name: "rolled back from", setup: func(t *testing.T, r *rig) *protobuf.Generation {
			r.deployMain(t, "m1")
			r.deployMain(t, "m2")
			r.exec.set("/nix/store/m1")
			return mainGeneration("m3", "/nix/store/m2")
		}},
		{name: "system turns foreign after the take", flip: true, setup: func(t *testing.T, r *rig) *protobuf.Generation {
			r.deployMain(t, "m1")
			return mainGeneration("m2", "/nix/store/m2")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, t.TempDir())
			r.m.SetPollPeriod(time.Hour)
			var armed atomic.Bool
			newer := evalAt(mainGeneration("m9", "/nix/store/m9"), "gen-newer", 200)
			r.m.executor = &hookExecutor{controllableExecutor: r.exec, onCurrent: func() {
				if armed.Load() && r.m.peekDeferred() == nil && armed.CompareAndSwap(true, false) {
					r.forceDeferred(newer)
					if c.flip {
						r.exec.set("/nix/store/foreign")
					}
				}
			}}
			r.start(t)
			g := evalAt(c.setup(t, r), "gen-offered", 100)
			r.quiesce()
			r.forceDeferred(g)
			gate := r.holdDeploys()
			before := r.deployLog()
			armed.Store(true)
			r.m.offerDeferred()
			require.False(t, armed.Load(), "the generation was taken and decided on")
			assert.Equal(t, "gen-newer", r.deferredUuid())
			if c.deploys {
				waitFor(t, func() bool { return len(r.deployLog()) == len(before)+1 }, "the offered generation deploys")
			}
			r.releaseDeploys(gate)
			if c.deploys {
				r.waitDeploys(t, append(before, "m2/switch", "m9/switch")...)
				return
			}
			assert.Equal(t, before, r.deployLog())
		})
	}
}

// m1 runs; m1b is deploying and m2 queued behind it when a session lease
// appears and m3, evaluated after m2, is confirmed and deferred. m1b ends,
// the deployer refuses m2: m3 must stay deferred and deploy once the lease
// ends; m2 never deploys.
func TestAs4oOlderRefusalKeepsNewerDeferred(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.deployMain(t, "m1")
	gate := r.holdDeploys()
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m1b", "/nix/store/m1b")))
	waitFor(t, func() bool { return slices.Contains(r.deployLog(), "m1b/switch") }, "m1b in flight")
	require.Equal(t, "submitted:switch", r.fetchDeploy(evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	r.writeLease(t, "session")
	require.Equal(t, "skipped:lease-decision", r.fetchDeploy(evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	require.Equal(t, "gen-m3", r.deferredUuid())

	r.releaseDeploys(gate)
	waitFor(t, func() bool { return r.d.Idle() && r.processed() }, "m2 refused, nothing in flight")
	r.quiesce()
	assert.Equal(t, []string{"m1/switch", "m1b/switch"}, r.deployLog())
	assert.Equal(t, "gen-m3", r.deferredUuid(), "m2's refusal left m3 deferred")

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m3/switch")
}

// The same queue, but the lease ends before m1b does: m2 is admitted and
// deploys, and its admission leaves the deferred m3, which deploys next.
func TestDeferredAdmittedOlderLeavesNewerDeferred(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.start(t)
	r.deployMain(t, "m1")
	gate := r.holdDeploys()
	require.Equal(t, "submitted:switch", r.fetchDeploy(mainGeneration("m1b", "/nix/store/m1b")))
	waitFor(t, func() bool { return slices.Contains(r.deployLog(), "m1b/switch") }, "m1b in flight")
	require.Equal(t, "submitted:switch", r.fetchDeploy(evalAt(mainGeneration("m2", "/nix/store/m2"), "gen-m2", 100)))
	r.writeLease(t, "session")
	require.Equal(t, "skipped:lease-decision", r.fetchDeploy(evalAt(mainGeneration("m3", "/nix/store/m3"), "gen-m3", 200)))
	require.NoError(t, os.Remove(r.lease))

	r.releaseDeploys(gate)
	r.waitDeploys(t, "m1/switch", "m1b/switch", "m2/switch", "m3/switch")
}

// As TestAs4oOlderRefusalKeepsNewerDeferred and
// TestDeferredAdmittedOlderLeavesNewerDeferred through the fetcher, builder
// and a real repository: production stamps EvalStartedAt, the test sets none.
func TestDeferredOrderFromFetchedGenerations(t *testing.T) {
	for _, leaseEndsFirst := range []bool{false, true} {
		name := "lease ends after m2 is refused"
		want := []string{"m1/switch", "m1b/switch", "m3/switch"}
		if leaseEndsFirst {
			name = "lease ends before m2 starts"
			want = []string{"m1/switch", "m1b/switch", "m2/switch", "m3/switch"}
		}
		t.Run(name, func(t *testing.T) {
			remote := newGitRemote(t)
			tip := remote.commit("", "m1")
			remote.setBranch("main", tip)
			r := newGitRig(t, t.TempDir(), remote)
			r.start(t)
			r.fetch(t, "m1")
			r.waitDeploys(t, "m1/switch")

			next := func(name string) {
				tip = remote.commit(tip, name)
				remote.setBranch("main", tip)
				r.fetch(t, name)
			}
			gate := r.holdDeploys()
			next("m1b")
			waitFor(t, func() bool { return slices.Contains(r.named(), "m1b/switch") }, "m1b in flight")
			next("m2")
			waitFor(t, func() bool { _, queued := r.d.Activity(); return queued }, "m2 queued")
			r.writeLease(t, "session")
			next("m3")
			r.waitMainDecided(t, tip)
			require.Equal(t, tip, r.deferredCommit())

			if leaseEndsFirst {
				require.NoError(t, os.Remove(r.lease))
			}
			r.releaseDeploys(gate)
			if !leaseEndsFirst {
				waitFor(t, func() bool { return r.d.Idle() && r.processed() }, "m2 refused, nothing in flight")
				r.quiesce()
				assert.Equal(t, []string{"m1/switch", "m1b/switch"}, r.named())
				assert.Equal(t, tip, r.deferredCommit(), "m2's refusal left m3 deferred")
				require.NoError(t, os.Remove(r.lease))
			}
			r.waitDeploys(t, want...)
		})
	}
}
