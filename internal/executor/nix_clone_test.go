package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitConfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

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

func fetchPrune(t *testing.T, r *git.Repository) {
	t.Helper()
	err := r.Fetch(&git.FetchOptions{RemoteName: "origin", Prune: true})
	if err != git.NoErrAlreadyUpToDate {
		require.NoError(t, err)
	}
}

func requireClones(t *testing.T, dir string, commitId plumbing.Hash, value string) {
	t.Helper()
	tmp, err := cloneRepoToTemp(dir, commitId.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	content, err := os.ReadFile(filepath.Join(tmp, "value"))
	require.NoError(t, err)
	require.Equal(t, value, string(content))
}

func TestCloneRepoToTempWithoutCheckout(t *testing.T) {
	srcDir := t.TempDir()
	src, err := git.PlainInit(srcDir, false)
	require.NoError(t, err)
	one := commitValue(t, src, srcDir, "one")

	// comin only fetches, so its HEAD is unborn.
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	_, err = r.CreateRemote(&gitConfig.RemoteConfig{Name: "origin", URLs: []string{srcDir}})
	require.NoError(t, err)
	fetchPrune(t, r)
	_, err = r.Head()
	require.Error(t, err, "the repository has no checkout")
	requireClones(t, dir, one, "one")

	// An older comin checked out the commit it selected, then main moved.
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.Checkout(&git.CheckoutOptions{Hash: one, Force: true}))
	two := commitValue(t, src, srcDir, "two")
	fetchPrune(t, r)
	requireClones(t, dir, two, "two")
	requireClones(t, dir, one, "one")

	// An older comin checked out a testing commit, then testing was pruned.
	srcW, err := src.Worktree()
	require.NoError(t, err)
	require.NoError(t, srcW.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("testing"), Create: true}))
	three := commitValue(t, src, srcDir, "three")
	require.NoError(t, srcW.Checkout(&git.CheckoutOptions{Branch: plumbing.Master}))
	fetchPrune(t, r)
	require.NoError(t, w.Checkout(&git.CheckoutOptions{Hash: three, Force: true}))
	require.NoError(t, src.Storer.RemoveReference(plumbing.NewBranchReferenceName("testing")))
	fetchPrune(t, r)
	_, err = r.Reference(plumbing.NewRemoteReferenceName("origin", "testing"), false)
	require.Error(t, err, "testing is pruned")
	requireClones(t, dir, two, "two")
	requireClones(t, dir, three, "three")
}
