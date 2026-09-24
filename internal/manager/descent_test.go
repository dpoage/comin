package manager

import (
	"os"
	"path/filepath"
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
}

func newGitRemote(t *testing.T) *gitRemote {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	return &gitRemote{t: t, dir: dir, repo: repo}
}

// commit creates a commit adding file on top of parent ("" for a root
// commit) and returns its hash. It moves no branch.
func (g *gitRemote) commit(parent, file string) string {
	w, err := g.repo.Worktree()
	require.NoError(g.t, err)
	if parent != "" {
		require.NoError(g.t, w.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(parent), Force: true}))
	}
	require.NoError(g.t, os.WriteFile(filepath.Join(g.dir, file), []byte(file), 0644))
	_, err = w.Add(file)
	require.NoError(g.t, err)
	h, err := w.Commit(file, &git.CommitOptions{Author: &object.Signature{Name: "dev", Email: "dev@example.org", When: time.Unix(0, 0)}})
	require.NoError(g.t, err)
	// Like a hosted remote, HEAD names the main branch rather than the
	// commit the worktree was left on.
	require.NoError(g.t, g.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))))
	return h.String()
}

func (g *gitRemote) setBranch(branch, hash string) {
	require.NoError(g.t, g.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), plumbing.NewHash(hash))))
}

func (g *gitRemote) deleteBranch(branch string) {
	require.NoError(g.t, g.repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(branch)))
}

// newGitRig is a rig whose fetcher polls remote through the real
// repository package, with comin's own clone in dir/repository.
func newGitRig(t *testing.T, dir string, remote *gitRemote) *rig {
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
	return newRigWithRepository(t, dir, func(mainCommitId string) repository.Repository {
		repo, err := repository.New(cfg, mainCommitId, prometheus.New())
		require.NoError(t, err)
		return repo
	})
}

// fetch runs one poll of the remote and waits until a new generation of
// the commit it selects has been built (the store reloaded after a restart
// still holds the generations built before it).
func (r *rig) fetch(t *testing.T, selected string) {
	t.Helper()
	before := map[string]bool{}
	for _, g := range r.s.GetState().Generations {
		before[g.Uuid] = true
	}
	r.m.Fetcher.TriggerFetch([]string{"origin"})
	require.Eventually(t, func() bool {
		for _, g := range r.s.GetState().Generations {
			if !before[g.Uuid] && g.SelectedCommitId == selected && g.BuildStatus == store.Built.String() {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the fetch never built %s", selected)
	r.waitIdle(t)
}

// names maps the commit hashes of a deploy log back to the test's names.
func names(log []string, commits map[string]string) []string {
	out := make([]string, len(log))
	for i, e := range log {
		commit, op, _ := strings.Cut(e, "/")
		out[i] = commits[commit] + "/" + op
	}
	return out
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
	r.fetch(t, m1)

	m2 := remote.commit(m1, "m2")
	y := remote.commit(m2, "y")
	remote.setBranch("main", m2)
	remote.setBranch("testing-r1", y)
	r.fetch(t, y)

	assert.Equal(t, []string{"m1/switch", "y/test"}, names(r.deployLog(), map[string]string{m1: "m1", y: "y"}))
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
	r.fetch(t, m1)
	r.writeLease(t, "git")

	m2 := remote.commit(m1, "m2")
	y := remote.commit(m2, "y")
	remote.setBranch("main", m2)
	remote.setBranch("testing-r1", y)
	r.fetch(t, y)

	assert.Equal(t, []string{"m1/switch", "y/test"}, names(r.deployLog(), map[string]string{m1: "m1", y: "y"}))
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
	r.fetch(t, m1)
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
// then the lease ends and comin returns. comin's clone keeps the deleted
// branch (fetch does not prune), so after a restart the repository
// selects the released head again. Mutant: no released check in the
// deploy gate.
func TestC3ReleasedHeadOfDeletedBranchNotRedeployedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	remote := newGitRemote(t)
	m1 := remote.commit("", "m1")
	remote.setBranch("main", m1)
	r1 := newGitRig(t, dir, remote)
	r1.start(t)
	r1.fetch(t, m1)
	t1 := remote.commit(m1, "t1")
	remote.setBranch("testing-r1", t1)
	r1.fetch(t, t1)
	require.True(t, r1.leaseExists())

	remote.deleteBranch("testing-r1")
	require.NoError(t, os.Remove(r1.lease))
	r1.waitIdle(t)
	commits := map[string]string{m1: "m1", t1: "t1"}
	require.Equal(t, []string{"m1/switch", "t1/test", "m1/switch"}, names(r1.deployLog(), commits))
	r1.stop()

	r2 := newGitRig(t, dir, remote)
	r2.exec.set("/nix/store/" + m1)
	r2.start(t)
	r2.fetch(t, t1)
	assert.Empty(t, r2.deployLog())
	assert.False(t, r2.leaseExists())
	assert.Equal(t, "none", r2.drift().State)
}
