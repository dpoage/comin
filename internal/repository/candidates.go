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
	Verified                            bool   // false iff signing is required and no configured key signed CommitId
	SignedBy                            string // the signer's primary identity name when signed
}

// Candidates returns the deployable commits of a repository status, most
// preferred first: the testing head when a testing branch is selected,
// then the main commit. It reads only rs.
func Candidates(rs *pb.RepositoryStatus) []Candidate {
	verifiedSigned := !rs.SelectedCommitShouldBeSigned.GetValue()
	candidates := []Candidate{}

	// The testing head is a candidate only when selected: when it equals
	// the main head, upstream selects main and there is a single candidate.
	if rs.SelectedBranchIsTesting.GetValue() {
		testing := Candidate{
			Remote:         rs.SelectedRemoteName,
			Branch:         rs.SelectedBranchName,
			CommitId:       rs.SelectedCommitId,
			CommitMsg:      rs.SelectedCommitMsg,
			Testing:        true,
			MainCommitId:   rs.MainCommitId,
			MainRemoteName: rs.MainRemoteName,
			MainBranchName: rs.MainBranchName,
			Verified:       verifiedSigned,
		}
		if remote := GetRemote(rs, rs.SelectedRemoteName); remote != nil && remote.Testing != nil {
			testing.Verified = verifiedSigned || remote.Testing.Signed.GetValue()
			testing.SignedBy = remote.Testing.SignedBy
		}
		candidates = append(candidates, testing)
	}

	// The main candidate carries the remote and branch that really hold
	// main_commit_id: the first fetched remote whose main branch is at
	// that commit. rs.main_remote_name is not used here because it is
	// empty after a restart when the main head did not move.
	if rs.MainCommitId == "" {
		return candidates
	}
	for _, remote := range rs.Remotes {
		if remote.FetchErrorMsg != "" || remote.Main == nil {
			continue
		}
		if remote.Main.CommitId != rs.MainCommitId {
			continue
		}
		candidates = append(candidates, Candidate{
			Remote:         remote.Name,
			Branch:         remote.Main.Name,
			CommitId:       rs.MainCommitId,
			CommitMsg:      remote.Main.CommitMsg,
			MainCommitId:   rs.MainCommitId,
			MainRemoteName: rs.MainRemoteName,
			MainBranchName: rs.MainBranchName,
			Verified:       verifiedSigned || remote.Main.Signed.GetValue(),
			SignedBy:       remote.Main.SignedBy,
		})
		return candidates
	}
	return candidates
}
