package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/assert"
)

// initRemoteRepository creates a remote git repository with one commit
// on the main branch.
func initRemoteRepository(t *testing.T, dir string) {
	t.Helper()
	r, err := git.PlainInit(dir, false)
	assert.Nil(t, err)
	w, err := r.Worktree()
	assert.Nil(t, err)
	filename := filepath.Join(dir, "file-1")
	assert.Nil(t, os.WriteFile(filename, []byte("file-1"), 0644))
	_, err = w.Add("file-1")
	assert.Nil(t, err)
	_, err = w.Commit("file-1", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "John Doe",
			Email: "john@doe.org",
			When:  time.Unix(0, 0),
		},
	})
	assert.Nil(t, err)
}

func TestFetchRemotesRunsTheFirstFetchAtStart(t *testing.T) {
	r1Dir := t.TempDir()
	r2Dir := t.TempDir()
	initRemoteRepository(t, r1Dir)
	initRemoteRepository(t, r2Dir)

	gitConfig := types.GitConfig{
		Path: t.TempDir(),
		Remotes: []types.Remote{
			{
				Name: "r1",
				URL:  r1Dir,
				Branches: types.Branches{
					Main: types.Branch{Name: "main"},
				},
				Timeout: 30,
				Poller:  types.Poller{Period: 3600},
			},
			{
				Name: "r2",
				URL:  r2Dir,
				Branches: types.Branches{
					Main: types.Branch{Name: "main"},
				},
				Timeout: 30,
				Poller:  types.Poller{Period: 7200},
			},
		},
	}
	repo, err := repository.New(gitConfig, "", prometheus.New())
	assert.Nil(t, err)
	f := fetcher.NewFetcher(repo)
	f.Start(t.Context())

	s := New()
	s.FetchRemotes(f, gitConfig.Remotes)

	// Both periods are an hour or more: nothing would be fetched within
	// the test horizon if the first run waited one period.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		latest := f.Latest()
		for _, remote := range latest.Remotes {
			assert.True(c, remote.GetFetched().GetValue(), "the remote %s has not been fetched at start", remote.Name)
			assert.Empty(c, remote.FetchErrorMsg)
		}
	}, 5*time.Second, 100*time.Millisecond, "the first fetch did not run at comin start")
}
