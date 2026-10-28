package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitConfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// cominStyleRepository returns a repository like the one comin keeps: it only
// ever fetches, so its HEAD is unborn, and returns the commit it fetched.
func cominStyleRepository(t *testing.T) (dir, commitId string) {
	t.Helper()
	srcDir := t.TempDir()
	src, err := git.PlainInit(srcDir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "default.nix"), []byte("{}"), 0o644))
	w, err := src.Worktree()
	require.NoError(t, err)
	_, err = w.Add("default.nix")
	require.NoError(t, err)
	hash, err := w.Commit("one", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@comin.space", When: time.Now()}})
	require.NoError(t, err)

	dir = t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	_, err = r.CreateRemote(&gitConfig.RemoteConfig{Name: "origin", URLs: []string{srcDir}})
	require.NoError(t, err)
	require.NoError(t, r.Fetch(&git.FetchOptions{RemoteName: "origin"}))
	_, err = r.Head()
	require.Error(t, err, "the repository has no checkout")
	return dir, hash.String()
}

// The clone NixLocal evaluates does not depend on the HEAD of comin's
// repository, which comin never checks out.
func TestCloneRepoToTempWithoutCheckout(t *testing.T) {
	dir, commitId := cominStyleRepository(t)
	tmp, err := cloneRepoToTemp(dir, commitId)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	_, err = os.Stat(filepath.Join(tmp, "default.nix"))
	require.NoError(t, err)
}
