package executor

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/nlewo/comin/internal/utils"
	"github.com/sirupsen/logrus"
)

type NixLocal struct{}

func NewNixExecutor() (*NixLocal, error) {
	return &NixLocal{}, nil
}

func (n *NixLocal) ReadMachineId() (string, error) {
	return utils.ReadMachineIdLinux()
}

func (n *NixLocal) IsStorePathExist(storePath string) bool {
	return isStorePathExist(storePath)
}

func (n *NixLocal) NeedToReboot(outPath, operation string) bool {
	return utils.NeedToRebootLinux(outPath, operation)
}
func (n *NixLocal) CurrentSystem() (string, error) {
	return currentSystem()
}

func (n *NixLocal) Eval(ctx context.Context, repositoryPath, repositorySubdir, commitId, systemAttr, hostname string) (drvPath string, outPath string, machineId string, err error) {
	tempDir, err := cloneRepoToTemp(repositoryPath, commitId)
	defer os.RemoveAll(tempDir) // nolint: errcheck
	if err != nil {
		return
	}
	logrus.Debugf("nix: temporary cloned into %s", tempDir)
	nixDir := path.Join(tempDir, repositorySubdir)
	return showDerivationWithNix(ctx, nixDir, systemAttr)
}

func (n *NixLocal) Build(ctx context.Context, drvPath string) (err error) {
	return buildWithNix(ctx, drvPath)
}

func (n *NixLocal) Deploy(ctx context.Context, outPath, operation string) (needToRestartComin bool, profilePath string, err error) {
	return deployLinux(ctx, outPath, operation)
}

// cloneRepoToTemp copies commitId and its tree, without a git transport: a
// go-git clone fails when the source HEAD is detached at a commit no ref names.
func cloneRepoToTemp(remoteDir string, commitId string) (string, error) {
	dir, err := os.MkdirTemp("", "comin-git-clone-*")
	if err != nil {
		return "", err
	}
	logrus.Debugf("nix: copying the commit '%s' of %s into the temporary directory '%s'", commitId, remoteDir, dir)
	remoteRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		return "", fmt.Errorf("nix: failed to open the repository '%s': %s", remoteDir, err)
	}
	hash := plumbing.NewHash(commitId)
	commit, err := remoteRepo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("nix: failed to read the commit '%s' in '%s': %s", commitId, remoteDir, err)
	}
	r, err := git.PlainInit(dir, false)
	if err != nil {
		return "", fmt.Errorf("nix: failed to init a repository in '%s': %s", dir, err)
	}
	if err := copyObject(remoteRepo.Storer, r.Storer, hash); err != nil {
		return "", fmt.Errorf("nix: failed to copy the commit '%s' into '%s': %s", commitId, dir, err)
	}
	if err := copyTree(remoteRepo.Storer, r.Storer, commit.TreeHash, map[plumbing.Hash]bool{}); err != nil {
		return "", fmt.Errorf("nix: failed to copy the tree of the commit '%s' into '%s': %s", commitId, dir, err)
	}
	// The parents are not copied.
	if err := r.Storer.SetShallow([]plumbing.Hash{hash}); err != nil {
		return "", fmt.Errorf("nix: failed to mark '%s' as shallow: %s", dir, err)
	}
	w, err := r.Worktree()
	if err != nil {
		return "", err
	}
	err = w.Checkout(&git.CheckoutOptions{
		Hash:  hash,
		Force: true,
	})
	if err != nil {
		return "", fmt.Errorf("nix: failed to checkout the commit '%s' in '%s': %s", commitId, dir, err)
	}
	return dir, nil
}

func copyObject(src, dst storer.EncodedObjectStorer, hash plumbing.Hash) error {
	o, err := src.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return err
	}
	_, err = dst.SetEncodedObject(o)
	return err
}

// copyTree copies the tree hash with its subtrees and blobs. Submodule
// commits are not in the repository, so they are skipped.
func copyTree(src, dst storer.EncodedObjectStorer, hash plumbing.Hash, copied map[plumbing.Hash]bool) error {
	if copied[hash] {
		return nil
	}
	copied[hash] = true
	tree, err := object.GetTree(src, hash)
	if err != nil {
		return err
	}
	if err := copyObject(src, dst, hash); err != nil {
		return err
	}
	for _, e := range tree.Entries {
		switch {
		case e.Mode == filemode.Submodule:
		case e.Mode == filemode.Dir:
			if err := copyTree(src, dst, e.Hash, copied); err != nil {
				return err
			}
		case !copied[e.Hash]:
			copied[e.Hash] = true
			if err := copyObject(src, dst, e.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}
