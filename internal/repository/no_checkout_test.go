package repository

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headFile is what a concurrent reader of the repository, such as
// `nix eval git+file://…?rev=…`, depends on staying untouched.
type headFile struct {
	content string
	info    os.FileInfo
}

func readHeadFile(t *testing.T, dir string) headFile {
	t.Helper()
	p := filepath.Join(dir, ".git", "HEAD")
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	info, err := os.Stat(p)
	require.NoError(t, err)
	return headFile{content: string(b), info: info}
}

func assertHeadUntouched(t *testing.T, dir string, before headFile, when string) {
	t.Helper()
	after := readHeadFile(t, dir)
	assert.Equal(t, before.content, after.content, "%s: .git/HEAD content", when)
	assert.True(t, os.SameFile(before.info, after.info), "%s: .git/HEAD was replaced", when)
	assert.Equal(t, before.info.ModTime(), after.info.ModTime(), "%s: .git/HEAD was written", when)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{".git"}, names, "%s: the working tree was written", when)
}

// No fetch writes .git/HEAD or the working tree, whether the selection
// changes or not, and a repository comin just initialized keeps the unborn
// HEAD it started with.
func TestUpdateNeverChecksOut(t *testing.T) {
	r1Dir := t.TempDir()
	r1, _ := initRemoteRepostiory(r1Dir, true)
	r := newCominRepository(t, "", originRemote("origin", r1Dir))
	dir := r.GitConfig.Path
	initial := readHeadFile(t, dir)
	_, err := r.Repository.Head()
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "a fresh comin repository has no checkout")

	update := func(when, branch string) {
		t.Helper()
		r.Fetch([]string{"origin"})
		require.NoError(t, r.Update())
		assert.Equal(t, branch, r.RepositoryStatus.SelectedBranchName, when)
		assertHeadUntouched(t, dir, initial, when)
	}

	update("first selection", "main")
	for range 3 {
		update("unchanged selection", "main")
	}
	mainMoved, err := commitFile(r1, r1Dir, "main", "file-4")
	require.NoError(t, err)
	update("main moves", "main")
	assert.Equal(t, mainMoved, r.RepositoryStatus.SelectedCommitId)
	require.NoError(t, r1.Storer.SetReference(plumbing.NewHashReference("refs/heads/testing", plumbing.NewHash(mainMoved))))
	testingCommit, err := commitFile(r1, r1Dir, "testing", "file-5")
	require.NoError(t, err)
	update("testing is selected", "testing")
	assert.Equal(t, testingCommit, r.RepositoryStatus.SelectedCommitId)
	update("testing stays selected", "testing")
	_, err = r.Repository.Head()
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

// An Update that selects nothing fails and says so.
func TestUpdateWithoutSelection(t *testing.T) {
	r := newCominRepository(t, "", originRemote("origin", t.TempDir()))
	r.Fetch([]string{"origin"})
	assert.NotEqual(t, "", GetRemote(r.RepositoryStatus, "origin").FetchErrorMsg)
	assert.ErrorContains(t, r.Update(), "no commit is selected")
	assert.Equal(t, "no commit is selected", r.RepositoryStatus.ErrorMsg)
	assert.Equal(t, "", r.RepositoryStatus.SelectedCommitId)
}
