package repository

import (
	"os"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/assert"
)

// originRemote returns the remote configuration of a comin repository
// pointing at url, with a main and a testing branch.
func originRemote(name, url string) types.Remote {
	return types.Remote{
		Name: name,
		URL:  url,
		Branches: types.Branches{
			Main: types.Branch{
				Name: "main",
			},
			Testing: types.Branch{
				Name: "testing",
			},
		},
		Timeout: 30,
	}
}

// newCominRepository opens a comin repository on a scratch directory with
// the persisted mainCommitId.
func newCominRepository(t *testing.T, mainCommitId string, remotes ...types.Remote) *repository {
	t.Helper()
	gitConfig := types.GitConfig{
		Path:    t.TempDir(),
		Remotes: remotes,
	}
	r, err := New(gitConfig, mainCommitId, prometheus.New())
	assert.Nil(t, err)
	return r
}

// headOfBranch returns the commit id and message of a branch of a remote
// repository.
func headOfBranch(t *testing.T, r *git.Repository, branch string) (string, string) {
	t.Helper()
	ref, err := r.Reference(plumbing.NewBranchReferenceName(branch), true)
	assert.Nil(t, err)
	commit, err := r.CommitObject(ref.Hash())
	assert.Nil(t, err)
	return ref.Hash().String(), commit.Message
}

// TestCandidates walks the repository state matrix and asserts the
// ordered candidate list derived from the repository status: the testing
// head when testing is selected, then the main commit — including the
// main candidate while testing is selected.
func TestCandidates(t *testing.T) {
	t.Run("main only", func(t *testing.T) {
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, false)
		cMain, msgMain := headOfBranch(t, r1, "main")

		r := newCominRepository(t, "", originRemote("origin", r1Dir))
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		assert.Equal(t, []Candidate{{
			Remote: "origin", Branch: "main",
			CommitId: cMain, CommitMsg: msgMain,
			MainCommitId:   cMain,
			MainRemoteName: "origin", MainBranchName: "main",
			Verified: true,
		}}, Candidates(r.GetRepositoryStatus()))
	})

	t.Run("main and testing on top", func(t *testing.T) {
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, true)
		cMain, msgMain := headOfBranch(t, r1, "main")
		_, _ = commitFile(r1, r1Dir, "testing", "file-4")
		cTesting, msgTesting := headOfBranch(t, r1, "testing")

		r := newCominRepository(t, "", originRemote("origin", r1Dir))
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		assert.Equal(t, []Candidate{
			{
				Remote: "origin", Branch: "testing",
				CommitId: cTesting, CommitMsg: msgTesting,
				Testing:        true,
				MainCommitId:   cMain,
				MainRemoteName: "origin", MainBranchName: "main",
				Verified: true,
			},
			{
				Remote: "origin", Branch: "main",
				CommitId: cMain, CommitMsg: msgMain,
				MainCommitId:   cMain,
				MainRemoteName: "origin", MainBranchName: "main",
				Verified: true,
			},
		}, Candidates(r.GetRepositoryStatus()))
	})

	t.Run("testing equals main", func(t *testing.T) {
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, true)
		cMain, msgMain := headOfBranch(t, r1, "main")

		r := newCominRepository(t, "", originRemote("origin", r1Dir))
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		// Upstream does not select a testing head equal to the main
		// head: a single candidate.
		assert.Equal(t, []Candidate{{
			Remote: "origin", Branch: "main",
			CommitId: cMain, CommitMsg: msgMain,
			MainCommitId:   cMain,
			MainRemoteName: "origin", MainBranchName: "main",
			Verified: true,
		}}, Candidates(r.GetRepositoryStatus()))
	})

	t.Run("testing not descending from main", func(t *testing.T) {
		// main: c1 - c2 - c3
		// testing: c1 - c2 - cT
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, false)
		head, err := r1.Head()
		assert.Nil(t, err)
		commit, err := r1.CommitObject(head.Hash())
		assert.Nil(t, err)
		parents := commit.Parents()
		c2, err := parents.Next()
		assert.Nil(t, err)
		// Branch testing at the parent of the main head, then diverge.
		ref := plumbing.NewHashReference("refs/heads/testing", c2.Hash)
		assert.Nil(t, r1.Storer.SetReference(ref))
		_, _ = commitFile(r1, r1Dir, "testing", "file-t")
		cMain, msgMain := headOfBranch(t, r1, "main")

		r := newCominRepository(t, "", originRemote("origin", r1Dir))
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		assert.Equal(t, []Candidate{{
			Remote: "origin", Branch: "main",
			CommitId: cMain, CommitMsg: msgMain,
			MainCommitId:   cMain,
			MainRemoteName: "origin", MainBranchName: "main",
			Verified: true,
		}}, Candidates(r.GetRepositoryStatus()))
	})

	// The failing remote was fetched successfully earlier, so its stale
	// main.commit_id still equals main_commit_id: candidates must come
	// from the remote that fetched.
	t.Run("fetch error on one of two remotes", func(t *testing.T) {
		for _, withTesting := range []bool{false, true} {
			r1Dir := t.TempDir()
			r2Dir := t.TempDir()
			r1, _ := initRemoteRepostiory(r1Dir, withTesting)
			r2, _ := initRemoteRepostiory(r2Dir, withTesting)
			cMain, msgMain := headOfBranch(t, r2, "main")
			cTesting, msgTesting := "", ""
			if withTesting {
				_, _ = commitFile(r1, r1Dir, "testing", "file-4")
				_, _ = commitFile(r2, r2Dir, "testing", "file-4")
				// Identical commits: the stale head of the failing
				// remote also equals main_commit_id.
				c1, _ := headOfBranch(t, r1, "testing")
				c2, msg2 := headOfBranch(t, r2, "testing")
				cTesting, msgTesting = c2, msg2
				assert.Equal(t, c1, c2)
			}

			r := newCominRepository(t, "",
				originRemote("r1", r1Dir), originRemote("r2", r2Dir))
			r.Fetch([]string{"r1", "r2"})
			assert.Nil(t, r.Update())
			assert.Nil(t, os.RemoveAll(r1Dir))
			r.Fetch([]string{"r1", "r2"})
			assert.Nil(t, r.Update())

			expected := []Candidate{{
				Remote: "r2", Branch: "main",
				CommitId: cMain, CommitMsg: msgMain,
				MainCommitId:   cMain,
				MainRemoteName: "r1", MainBranchName: "main",
				Verified: true,
			}}
			if withTesting {
				expected = append([]Candidate{{
					Remote: "r2", Branch: "testing",
					CommitId: cTesting, CommitMsg: msgTesting,
					Testing:        true,
					MainCommitId:   cMain,
					MainRemoteName: "r1", MainBranchName: "main",
					Verified: true,
				}}, expected...)
			}
			assert.Equal(t, expected, Candidates(r.GetRepositoryStatus()))
		}
	})

	// After a restart with a persisted MainCommitId equal to the main
	// head, main_remote_name is empty: the main candidate carries the
	// remote that really holds the commit.
	t.Run("restart with persisted MainCommitId", func(t *testing.T) {
		for _, withTestingOnTop := range []bool{false, true} {
			r1Dir := t.TempDir()
			r1, _ := initRemoteRepostiory(r1Dir, true)
			cMain, msgMain := headOfBranch(t, r1, "main")
			cTesting, msgTesting := "", ""
			if withTestingOnTop {
				_, _ = commitFile(r1, r1Dir, "testing", "file-4")
				cTesting, msgTesting = headOfBranch(t, r1, "testing")
			}

			r := newCominRepository(t, cMain, originRemote("origin", r1Dir))

			// Before the first fetch, nothing is selected and no
			// remote carries the persisted commit yet.
			assert.Equal(t, []Candidate{}, Candidates(r.GetRepositoryStatus()))

			r.Fetch([]string{"origin"})
			assert.Nil(t, r.Update())

			expected := []Candidate{{
				Remote: "origin", Branch: "main",
				CommitId: cMain, CommitMsg: msgMain,
				MainCommitId: cMain,
				Verified:     true,
			}}
			if withTestingOnTop {
				expected = append([]Candidate{{
					Remote: "origin", Branch: "testing",
					CommitId: cTesting, CommitMsg: msgTesting,
					Testing:      true,
					MainCommitId: cMain,
					Verified:     true,
				}}, expected...)
			}
			assert.Equal(t, expected, Candidates(r.GetRepositoryStatus()))
		}
	})
}

// TestCandidatesSignatureVerdict asserts each candidate carries the
// signature verdict of its own commit and not HEAD's.
func TestCandidatesSignatureVerdict(t *testing.T) {
	f, _ := os.Open("./test.private")
	entityList, _ := openpgp.ReadArmoredKeyRing(f)
	entity := entityList[0]

	signedBy := "test <test@comin.space>"

	// {signed main, unsigned testing} -> main verified, testing not
	t.Run("signed main unsigned testing", func(t *testing.T) {
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, true)
		_, _ = commitFileAndSign(r1, r1Dir, "main", "file-4", entity)
		// Put testing on top of the signed main head, then commit
		// unsigned there.
		testingRef := plumbing.NewHashReference("refs/heads/testing", plumbing.NewHash(HeadCommitId(r1)))
		assert.Nil(t, r1.Storer.SetReference(testingRef))
		_, _ = commitFile(r1, r1Dir, "testing", "file-5")

		gitConfig := types.GitConfig{
			Path:              t.TempDir(),
			GpgPublicKeyPaths: []string{"./test.public"},
			Remotes:           []types.Remote{originRemote("origin", r1Dir)},
		}
		r, err := New(gitConfig, "", prometheus.New())
		assert.Nil(t, err)
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		candidates := Candidates(r.GetRepositoryStatus())
		assert.Equal(t, 2, len(candidates))
		// The unsigned testing head is selected, so it is HEAD.
		assert.False(t, candidates[0].Verified)
		assert.Equal(t, "", candidates[0].SignedBy)
		assert.True(t, candidates[1].Verified)
		assert.Equal(t, signedBy, candidates[1].SignedBy)
	})

	// {unsigned main, signed testing} -> testing verified, main not
	t.Run("unsigned main signed testing", func(t *testing.T) {
		r1Dir := t.TempDir()
		r1, _ := initRemoteRepostiory(r1Dir, true)
		_, _ = commitFileAndSign(r1, r1Dir, "testing", "file-4", entity)

		gitConfig := types.GitConfig{
			Path:              t.TempDir(),
			GpgPublicKeyPaths: []string{"./test.public"},
			Remotes:           []types.Remote{originRemote("origin", r1Dir)},
		}
		r, err := New(gitConfig, "", prometheus.New())
		assert.Nil(t, err)
		r.Fetch([]string{"origin"})
		assert.Nil(t, r.Update())

		candidates := Candidates(r.GetRepositoryStatus())
		assert.Equal(t, 2, len(candidates))
		// The signed testing head is selected, so it is HEAD.
		assert.True(t, candidates[0].Verified)
		assert.Equal(t, signedBy, candidates[0].SignedBy)
		assert.False(t, candidates[1].Verified)
		assert.Equal(t, "", candidates[1].SignedBy)
	})
}
