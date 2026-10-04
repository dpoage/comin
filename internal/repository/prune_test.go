package repository

import (
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/assert"
)

func localRef(t *testing.T, r *git.Repository, name string) (plumbing.Hash, error) {
	t.Helper()
	ref, err := r.Reference(plumbing.ReferenceName(name), false)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// A testing branch deleted on the remote (nothing else changes, so the
// fetch is a deletion-only fetch) must lose its remote-tracking ref, and
// a comin starting from an empty store must never select its old commit.
func TestFetchPrunesDeletedRemoteBranch(t *testing.T) {
	remoteDir := t.TempDir()
	cominDir := t.TempDir()
	remote, err := initRemoteRepostiory(remoteDir, true)
	assert.Nil(t, err)

	// testing: main + one commit; testing-x: main + one other commit
	liveTesting, err := commitFile(remote, remoteDir, "testing", "file-testing")
	assert.Nil(t, err)
	err = remote.Storer.SetReference(plumbing.NewHashReference("refs/heads/testing-x", plumbing.NewHash(mustMainHash(t, remote))))
	assert.Nil(t, err)
	endedCommit, err := commitFile(remote, remoteDir, "testing-x", "file-ended")
	assert.Nil(t, err)
	// the remote HEAD leaves testing-x before it is deleted
	w, err := remote.Worktree()
	assert.Nil(t, err)
	assert.Nil(t, w.Checkout(&git.CheckoutOptions{Branch: "refs/heads/main", Force: true}))
	mainCommit := mustMainHash(t, remote)

	gitConfig := types.GitConfig{
		Path: cominDir,
		Remotes: []types.Remote{{
			Name: "origin",
			URL:  remoteDir,
			Branches: types.Branches{
				Main:    types.Branch{Name: "main"},
				Testing: types.Branch{Name: "testing-x"},
			},
			Timeout: 30,
		}},
	}
	r, err := New(gitConfig, "", prometheus.New())
	assert.Nil(t, err)
	r.Fetch([]string{"origin"})
	assert.Equal(t, "", r.RepositoryStatus.Remotes[0].FetchErrorMsg)
	h, err := localRef(t, r.Repository, "refs/remotes/origin/testing-x")
	assert.Nil(t, err)
	assert.Equal(t, endedCommit, h.String())
	assert.Nil(t, r.Update(nil))
	assert.Equal(t, endedCommit, r.RepositoryStatus.SelectedCommitId)

	// robot-override end: delete the branch on the remote, nothing else changes
	assert.Nil(t, remote.Storer.RemoveReference("refs/heads/testing-x"))

	r.Fetch([]string{"origin"})
	assert.Equal(t, "", r.RepositoryStatus.Remotes[0].FetchErrorMsg)
	_, err = localRef(t, r.Repository, "refs/remotes/origin/testing-x")
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)

	// P2: the refs that still exist on the remote are untouched
	h, err = localRef(t, r.Repository, "refs/remotes/origin/main")
	assert.Nil(t, err)
	assert.Equal(t, mainCommit, h.String())
	h, err = localRef(t, r.Repository, "refs/remotes/origin/testing")
	assert.Nil(t, err)
	assert.Equal(t, liveTesting, h.String())

	// comin restarted with an empty store (no store.json)
	r2, err := New(gitConfig, "", prometheus.New())
	assert.Nil(t, err)
	r2.Fetch([]string{"origin"})
	assert.Nil(t, r2.Update(nil))
	assert.Contains(t, r2.RepositoryStatus.Remotes[0].Testing.ErrorMsg, "doesn't exist")
	assert.Equal(t, "", r2.RepositoryStatus.Remotes[0].Testing.CommitId)
	assert.Equal(t, mainCommit, r2.RepositoryStatus.SelectedCommitId)
	assert.Equal(t, "main", r2.RepositoryStatus.SelectedBranchName)
	assert.NotEqual(t, endedCommit, r2.RepositoryStatus.SelectedCommitId)
}

func mustMainHash(t *testing.T, r *git.Repository) string {
	t.Helper()
	ref, err := r.Reference("refs/heads/main", true)
	assert.Nil(t, err)
	return ref.Hash().String()
}
