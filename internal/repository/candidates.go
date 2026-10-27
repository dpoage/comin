package repository

import (
	pb "github.com/nlewo/comin/internal/protobuf"
)

// Candidate is a commit comin may evaluate and deploy.
type Candidate struct {
	Remote, Branch, CommitId, CommitMsg string
	Testing                             bool
	MainCommitId                        string // the main commit the candidate sits on (copied into the generation)
	MainRemoteName, MainBranchName      string // copied from rs.main_remote_name/main_branch_name (generation fields)
	// Verified = !required || (a verdict exists for exactly CommitId && it
	// is signed), where required is rs.selected_commit_should_be_signed, set
	// from the configured GPG keys when the status is created.
	Verified bool
	SignedBy string // the signer's primary identity name when signed
}

// Candidates returns the deployable commits of a repository status, most
// preferred first: the testing head when a testing branch is selected,
// then the main commit. The main commit is taken from the first remote in
// rs.remotes order holding it without a fetch error, else from the first
// remote holding it. It reads only rs.
func Candidates(rs *pb.RepositoryStatus) []Candidate {
	candidates := []Candidate{}

	if rs.SelectedBranchIsTesting.GetValue() {
		var testing *pb.Branch
		if remote := GetRemote(rs, rs.SelectedRemoteName); remote != nil {
			testing = remote.Testing
		}
		verified, signedBy := verdict(rs, testing, rs.SelectedCommitId)
		candidates = append(candidates, Candidate{
			Remote:         rs.SelectedRemoteName,
			Branch:         rs.SelectedBranchName,
			CommitId:       rs.SelectedCommitId,
			CommitMsg:      rs.SelectedCommitMsg,
			Testing:        true,
			MainCommitId:   rs.MainCommitId,
			MainRemoteName: rs.MainRemoteName,
			MainBranchName: rs.MainBranchName,
			Verified:       verified,
			SignedBy:       signedBy,
		})
	}

	// rs.main_remote_name is empty after a restart when the main head did
	// not move, so the remote is found by its main commit.
	remote := mainRemote(rs)
	if remote == nil {
		return candidates
	}
	verified, signedBy := verdict(rs, remote.Main, rs.MainCommitId)
	return append(candidates, Candidate{
		Remote:         remote.Name,
		Branch:         remote.Main.Name,
		CommitId:       rs.MainCommitId,
		CommitMsg:      remote.Main.CommitMsg,
		MainCommitId:   rs.MainCommitId,
		MainRemoteName: rs.MainRemoteName,
		MainBranchName: rs.MainBranchName,
		Verified:       verified,
		SignedBy:       signedBy,
	})
}

// mainRemote returns the remote holding rs.main_commit_id, preferring one
// without a fetch error, or nil.
func mainRemote(rs *pb.RepositoryStatus) *pb.Remote {
	if rs.MainCommitId == "" {
		return nil
	}
	var fallback *pb.Remote
	for _, remote := range rs.Remotes {
		if remote.Main.GetCommitId() != rs.MainCommitId {
			continue
		}
		if remote.FetchErrorMsg == "" {
			return remote
		}
		if fallback == nil {
			fallback = remote
		}
	}
	return fallback
}

// verdict returns Verified and SignedBy of commitId: without a signed
// verdict branch holds for commitId, only an unrequired signature verifies.
func verdict(rs *pb.RepositoryStatus, branch *pb.Branch, commitId string) (bool, string) {
	if branch.GetCommitId() == commitId && branch.GetSigned().GetValue() {
		return true, branch.SignedBy
	}
	return !rs.SelectedCommitShouldBeSigned.GetValue(), ""
}
