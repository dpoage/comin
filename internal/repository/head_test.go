package repository

import (
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/require"
)

func requireUnbornHead(t *testing.T, dir string) {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := r.Storer.Reference(plumbing.HEAD)
	require.NoError(t, err)
	require.Equal(t, plumbing.SymbolicReference, head.Type())
	require.Equal(t, plumbing.Master, head.Target())
	_, err = r.Head()
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

func TestNewUnbornsDetachedHead(t *testing.T) {
	remoteDir := t.TempDir()
	remote, err := initRemoteRepostiory(remoteDir, true)
	require.NoError(t, err)
	gitConfig := types.GitConfig{
		Path:    t.TempDir(),
		Remotes: []types.Remote{originRemote("origin", remoteDir)},
	}
	r, err := New(gitConfig, "", prometheus.New())
	require.NoError(t, err)
	requireUnbornHead(t, gitConfig.Path)
	r.Fetch([]string{"origin"})
	require.NoError(t, r.Update())

	// An older comin left HEAD detached at its selection.
	w, err := r.Repository.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(r.RepositoryStatus.SelectedCommitId), Force: true}))
	_, err = commitFile(remote, remoteDir, "main", "file-4")
	require.NoError(t, err)

	r, err = New(gitConfig, "", prometheus.New())
	require.NoError(t, err)
	requireUnbornHead(t, gitConfig.Path)
	r.Fetch([]string{"origin"})
	require.NoError(t, r.Update())
	requireUnbornHead(t, gitConfig.Path)
}
