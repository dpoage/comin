package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/require"
)

var mainBranch = plumbing.NewBranchReferenceName("main")

func commitValue(t *testing.T, src *git.Repository, srcDir, value string) plumbing.Hash {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "value"), []byte(value), 0o644))
	w, err := src.Worktree()
	require.NoError(t, err)
	_, err = w.Add("value")
	require.NoError(t, err)
	hash, err := w.Commit(value, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@comin.space", When: time.Now()}})
	require.NoError(t, err)
	return hash
}

// startComin opens the state dir as comin does at start, then fetches and
// selects; it returns the selected commit.
func startComin(t *testing.T, remoteDir, stateDir string) plumbing.Hash {
	t.Helper()
	r, err := repository.New(types.GitConfig{
		Path: stateDir,
		Remotes: []types.Remote{{
			Name:     "origin",
			URL:      remoteDir,
			Timeout:  30,
			Branches: types.Branches{Main: types.Branch{Name: "main"}, Testing: types.Branch{Name: "testing"}},
		}},
	}, "", prometheus.New())
	require.NoError(t, err)
	r.Fetch([]string{"origin"})
	require.NoError(t, r.Update())
	return plumbing.NewHash(r.GetRepositoryStatus().SelectedCommitId)
}

// olderCominCheckout detaches HEAD at hash and fills the worktree, as an older comin did.
func olderCominCheckout(t *testing.T, stateDir string, hash plumbing.Hash) {
	t.Helper()
	r, err := git.PlainOpen(stateDir)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.Checkout(&git.CheckoutOptions{Hash: hash, Force: true}))
}

// commitCount counts the commits reachable from hash that dir holds.
func commitCount(t *testing.T, dir string, hash plumbing.Hash) int {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	iter, err := r.Log(&git.LogOptions{From: hash})
	require.NoError(t, err)
	n := 0
	err = iter.ForEach(func(*object.Commit) error { n++; return nil })
	if err != plumbing.ErrObjectNotFound {
		require.NoError(t, err)
	}
	return n
}

// requireEvaluable asserts NixLocal gets commit's tree and its full history,
// which builtins.fetchGit reads as revCount.
func requireEvaluable(t *testing.T, stateDir string, commit plumbing.Hash, value string) {
	t.Helper()
	tmp, err := cloneRepoToTemp(stateDir, commit.String())
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(tmp, "value"))
	require.NoError(t, err)
	require.Equal(t, value, string(content))
	require.Equal(t, commitCount(t, stateDir, commit), commitCount(t, tmp, commit), "history of %s", commit)
}

func TestNixLocalOnUpgradedStateDir(t *testing.T) {
	remoteDir := t.TempDir()
	src, err := git.PlainInitWithOptions(remoteDir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: mainBranch}})
	require.NoError(t, err)
	one := commitValue(t, src, remoteDir, "one")
	stateDir := t.TempDir()

	require.Equal(t, one, startComin(t, remoteDir, stateDir))
	requireEvaluable(t, stateDir, one, "one")

	// An older comin checked out its selection, then main moved.
	olderCominCheckout(t, stateDir, one)
	two := commitValue(t, src, remoteDir, "two")
	require.Equal(t, two, startComin(t, remoteDir, stateDir))
	requireEvaluable(t, stateDir, two, "two")
	requireEvaluable(t, stateDir, one, "one")

	// An older comin checked out a testing commit, then testing was pruned.
	srcW, err := src.Worktree()
	require.NoError(t, err)
	require.NoError(t, srcW.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("testing"), Create: true}))
	three := commitValue(t, src, remoteDir, "three")
	require.NoError(t, srcW.Checkout(&git.CheckoutOptions{Branch: mainBranch, Force: true}))
	require.Equal(t, three, startComin(t, remoteDir, stateDir))
	olderCominCheckout(t, stateDir, three)
	require.NoError(t, src.Storer.RemoveReference(plumbing.NewBranchReferenceName("testing")))
	require.Equal(t, two, startComin(t, remoteDir, stateDir))
	state, err := git.PlainOpen(stateDir)
	require.NoError(t, err)
	_, err = state.Reference(plumbing.NewRemoteReferenceName("origin", "testing"), false)
	require.Error(t, err, "testing is pruned")
	requireEvaluable(t, stateDir, two, "two")
	requireEvaluable(t, stateDir, three, "three")
}
