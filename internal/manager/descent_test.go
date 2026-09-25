package manager

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitRemote is the git remote a robot's comin polls: a repository with a
// worktree, so the test can commit on any parent and move any branch.
type gitRemote struct {
	t    *testing.T
	dir  string
	repo *git.Repository
	// names maps commit hashes to the test's names, for deploy logs.
	names map[string]string
}

func newGitRemote(t *testing.T) *gitRemote {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	return &gitRemote{t: t, dir: dir, repo: repo, names: map[string]string{}}
}

// commit creates a commit named name on top of parent ("" for a root
// commit) and returns its hash. It moves no branch.
func (g *gitRemote) commit(parent, name string) string {
	w, err := g.repo.Worktree()
	require.NoError(g.t, err)
	if parent != "" {
		require.NoError(g.t, w.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(parent), Force: true}))
	}
	require.NoError(g.t, os.WriteFile(filepath.Join(g.dir, name), []byte(name), 0644))
	_, err = w.Add(name)
	require.NoError(g.t, err)
	h, err := w.Commit(name, &git.CommitOptions{Author: &object.Signature{Name: "dev", Email: "dev@example.org", When: time.Unix(0, 0)}})
	require.NoError(g.t, err)
	// Like a hosted remote, HEAD names the main branch rather than the
	// commit the worktree was left on.
	require.NoError(g.t, g.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))))
	g.names[h.String()] = name
	return h.String()
}

func (g *gitRemote) setBranch(branch, hash string) {
	require.NoError(g.t, g.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), plumbing.NewHash(hash))))
}

func (g *gitRemote) deleteBranch(branch string) {
	require.NoError(g.t, g.repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(branch)))
}

// gitRig is a rig whose fetcher polls a gitRemote through the real
// repository package, with comin's own clone in dir/repository.
type gitRig struct {
	*rig
	remote *gitRemote
}

func newGitRig(t *testing.T, dir string, remote *gitRemote) *gitRig {
	cfg := types.GitConfig{
		Path: filepath.Join(dir, "repository"),
		Remotes: []types.Remote{{
			Name:    "origin",
			URL:     remote.dir,
			Timeout: 30,
			Branches: types.Branches{
				Main:    types.Branch{Name: "main"},
				Testing: types.Branch{Name: "testing-r1"},
			},
		}},
	}
	r := newRigWithRepository(t, dir, func(mainCommitId string) repository.Repository {
		repo, err := repository.New(cfg, mainCommitId, prometheus.New())
		require.NoError(t, err)
		return repo
	})
	return &gitRig{rig: r, remote: remote}
}

// fetch runs one poll of the remote and waits until a new generation of
// the commit named selected has been built (the store reloaded after a
// restart still holds the generations built before it).
func (r *gitRig) fetch(t *testing.T, selected string) {
	t.Helper()
	before := map[string]bool{}
	for _, g := range r.s.GetState().Generations {
		before[g.Uuid] = true
	}
	r.m.Fetcher.TriggerFetch([]string{"origin"})
	waitFor(t, func() bool {
		for _, g := range r.s.GetState().Generations {
			if !before[g.Uuid] && r.remote.names[g.SelectedCommitId] == selected && g.BuildStatus == store.Built.String() {
				return true
			}
		}
		return false
	}, "the fetch never built %s", selected)
}

// fetchNoChange runs one poll of the remote that selects what the previous
// one selected, so it emits nothing, and waits until its result reached
// the fetcher (its main head is mainHead).
func (r *gitRig) fetchNoChange(t *testing.T, mainHead string) {
	t.Helper()
	r.m.Fetcher.TriggerFetch([]string{"origin"})
	waitFor(t, func() bool { return r.remote.names[r.m.Fetcher.MainHead()] == mainHead }, "the fetch never saw main %s", mainHead)
}

// named is the deploy log with commit hashes replaced by the test's names.
func (r *gitRig) named() []string {
	log := r.deployLog()
	out := make([]string, len(log))
	for i, e := range log {
		commit, op, _ := strings.Cut(e, "/")
		out[i] = r.remote.names[commit] + "/" + op
	}
	return out
}

// waitDeploys is rig.waitDeploys on the named deploy log.
func (r *gitRig) waitDeploys(t *testing.T, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	ok := assert.Eventually(t, func() bool {
		return slices.Equal(r.named(), want) && r.d.Idle() && r.processed()
	}, waitDeadline, 5*time.Millisecond)
	if !ok {
		t.Fatalf("deploy log %v, want %v (idle=%v)", r.named(), want, r.d.Idle())
	}
}

func (r *gitRig) current() string {
	return r.remote.names[strings.TrimPrefix(r.exec.get(), "/nix/store/")]
}

// ---------------------------------------------------------------------
// C2: descent
// ---------------------------------------------------------------------

// No lease, main and testing advance together (a robot lagging its tier
// starting an override): the testing head deploys, as on be6025e. Mutant:
// the descent gate applied with no lease (bef80fd).
func TestC2NoLeaseTestingOnNewerMainDeploys(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")

	m2 := remote.commit(m1, "m2")
	y := remote.commit(m2, "y")
	remote.setBranch("main", m2)
	remote.setBranch("testing-r1", y)
	r.fetch(t, "y")
	r.waitDeploys(t, "m1/switch", "y/test")
}

// A live git lease holds main at m1; the developer rebuilds the testing
// branch on the newer tier head m2. m1 is an ancestor of the testing head,
// so it deploys. Mutant: MainCommitId equality instead of git ancestry.
func TestC2GitLeaseTestingOnNewerMainDescendingFromHeldDeploys(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	r.writeLease(t, "git")

	m2 := remote.commit(m1, "m2")
	y := remote.commit(m2, "y")
	remote.setBranch("main", m2)
	remote.setBranch("testing-r1", y)
	r.fetch(t, "y")
	r.waitDeploys(t, "m1/switch", "y/test")
}

// A live git lease holds main at m1; a testing head whose history does not
// contain m1 is refused. Mutant: no ancestry check.
func TestC2GitLeaseTestingNotDescendingFromHeldRefused(t *testing.T) {
	remote := newGitRemote(t)
	r0 := remote.commit("", "r0")
	m1 := remote.commit(r0, "m1")
	z := remote.commit(r0, "z")
	remote.setBranch("main", m1)
	remote.setBranch("other", z)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	r.writeLease(t, "git")

	_, ok := r.m.leaseDeployDecision(testingGeneration(z, r0))
	assert.False(t, ok)
	_, ok = r.m.leaseDeployDecision(testingGeneration(remote.commit(m1, "y"), r0))
	assert.False(t, ok, "y is not in comin's clone yet: an ancestry error refuses")
}

// P-DESCENT: under a git lease the testing selection is relative to the
// held main commit. The tier moves to m2 while the lease is held, then the
// override is extended with a commit that still descends from m1 only: it
// deploys. Mutant: testing selection relative to the fetched main head.
func TestC2GitLeaseExtendAfterTierMovedDeploys(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r.fetch(t, "t1")
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists())

	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	r.fetchNoChange(t, "m2")

	t1b := remote.commit(t1, "t1-extend")
	remote.setBranch("testing-r1", t1b)
	r.fetch(t, "t1-extend")
	r.waitDeploys(t, "m1/switch", "t1/test", "t1-extend/test")
	r.waitDrift(t, "held")
}

// ---------------------------------------------------------------------
// P-TIER: after an override ends, the robot converges to its tier head
// ---------------------------------------------------------------------

// The robot deployed m1 and lags its tier: the remote main is m2 and the
// override's testing head y is built on m2, so m2 only ever arrived
// through the testing branch. When the override ends, comin returns to
// the last main it deployed (m2 is not built), then deploys m2 once.
// Mutant: offer only the deferred generation (no released-head exclusion
// in the testing selection).
func TestTierHeadCarriedByTestingDeploysAfterOverrideEnds(t *testing.T) {
	cases := []struct {
		name          string
		leaseFirst    bool
		deleteBranch  bool
		restartBefore bool
	}{
		{name: "first deploy without a lease"},
		{name: "first deploy with a git lease", leaseFirst: true},
		{name: "restart before the tier head deploys", restartBefore: true},
		{name: "remote branch deleted", deleteBranch: true},
		{name: "remote branch deleted, then a restart", deleteBranch: true, restartBefore: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			remote := newGitRemote(t)
			m1 := remote.commit("", "m1")
			remote.setBranch("main", m1)
			r := newGitRig(t, dir, remote)
			r.start(t)
			r.fetch(t, "m1")
			r.waitDeploys(t, "m1/switch")
			if c.leaseFirst {
				r.writeLease(t, "git")
			}

			m2 := remote.commit(m1, "m2")
			y := remote.commit(m2, "y")
			remote.setBranch("main", m2)
			remote.setBranch("testing-r1", y)
			r.fetch(t, "y")
			r.waitDeploys(t, "m1/switch", "y/test")
			require.True(t, r.leaseExists())

			if c.deleteBranch {
				remote.deleteBranch("testing-r1")
			}
			require.NoError(t, os.Remove(r.lease))
			r.waitDeploys(t, "m1/switch", "y/test", "m1/switch")

			want := []string{"m1/switch", "y/test", "m1/switch", "m2/switch"}
			if c.restartBefore {
				r.stop()
				r = newGitRig(t, dir, remote)
				r.exec.set("/nix/store/" + m1)
				r.start(t)
				want = []string{"m2/switch"}
			}
			r.fetch(t, "m2")
			r.waitDeploys(t, want...)
			r.waitDrift(t, "none")
			assert.Equal(t, "m2", r.current())
		})
	}
}

// The fetched main head was built while the git lease held it back, so the
// return goes straight to it instead of passing through the last deployed
// main. Mutant: always return to S4 expected.
func TestReturnGoesStraightToBuiltTierHead(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r.fetch(t, "t1")
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r.leaseExists())

	// The developer resets the testing branch to the held commit, so the
	// next poll selects the new tier head m2, which is built and held
	// back by the lease.
	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	remote.setBranch("testing-r1", m1)
	r.fetch(t, "m2")
	waitFor(t, func() bool {
		r.m.deferredMu.Lock()
		defer r.m.deferredMu.Unlock()
		return r.m.deferred != nil && r.m.deferred.SelectedCommitId == m2
	}, "m2 held back by the lease")

	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "t1/test", "m2/switch")
	r.waitDrift(t, "none")
}

// Ruling 3: once the override is released, the next poll selects the main
// head again, which is the system comin just returned to. It is not
// activated a second time. The later tier commit proves the pipeline has
// handled that poll. Mutant: no "already deployed" rule in the gate.
func TestMainReemittedAfterReturnIsNotRedeployed(t *testing.T) {
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r := newGitRig(t, t.TempDir(), remote)
	r.start(t)
	r.fetch(t, "m1")
	r.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r.fetch(t, "t1")
	r.waitDeploys(t, "m1/switch", "t1/test")
	require.NoError(t, os.Remove(r.lease))
	r.waitDeploys(t, "m1/switch", "t1/test", "m1/switch")

	r.fetch(t, "m1")
	waitFor(t, func() bool { return r.m.DeployConfirmer.status().Submitted == "" }, "the re-emitted m1 was confirmed")

	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	r.fetch(t, "m2")
	r.waitDeploys(t, "m1/switch", "t1/test", "m1/switch", "m2/switch")
}

// ---------------------------------------------------------------------
// C3: released head after the remote branch is deleted
// ---------------------------------------------------------------------

// R4's order: the developer's testing branch is deleted on the remote,
// then the lease ends and comin returns. comin's clone keeps the deleted
// branch (fetch does not prune); after a restart the released head is not
// selected again, the main head is, and it is already running.
func TestC3ReleasedHeadOfDeletedBranchNotRedeployedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r1 := newGitRig(t, dir, remote)
	r1.start(t)
	r1.fetch(t, "m1")
	r1.waitDeploys(t, "m1/switch")
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r1.fetch(t, "t1")
	r1.waitDeploys(t, "m1/switch", "t1/test")
	require.True(t, r1.leaseExists())

	remote.deleteBranch("testing-r1")
	require.NoError(t, os.Remove(r1.lease))
	r1.waitDeploys(t, "m1/switch", "t1/test", "m1/switch")
	r1.stop()

	r2 := newGitRig(t, dir, remote)
	r2.exec.set("/nix/store/" + m1)
	r2.start(t)
	r2.fetch(t, "m1")
	r2.waitDrift(t, "none")
	assert.Empty(t, r2.deployLog())
	assert.False(t, r2.leaseExists())
}
