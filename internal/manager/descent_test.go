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

// ---------------------------------------------------------------------
// C3: released head after the remote branch is deleted
// ---------------------------------------------------------------------

// R4's order: the developer's testing branch is deleted on the remote,
// then the lease ends and the operator switches back. comin's clone keeps
// the deleted branch (fetch does not prune), so after a restart the
// released head is still its branch head: the repository does not select
// it, and the next tier commit deploys. Mutant: the repository's testing
// selection does not exclude released heads.
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
	r1.waitDrift(t, "leaseless")
	require.NoError(t, r1.m.SwitchDeploymentLatest())
	r1.waitDeploys(t, "m1/switch", "t1/test", "m1/switch")
	r1.waitReleasedOnDisk(t)
	r1.stop()

	r2 := newGitRig(t, dir, remote)
	r2.exec.set("/nix/store/" + m1)
	r2.start(t)
	r2.fetch(t, "m1")
	m2 := remote.commit(m1, "m2")
	remote.setBranch("main", m2)
	r2.fetch(t, "m2")
	r2.waitDeploys(t, "m2/switch")
	r2.waitDrift(t, "none")
	assert.False(t, r2.leaseExists())
}
