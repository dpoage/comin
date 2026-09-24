package deployer

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	pb "github.com/nlewo/comin/internal/protobuf"

	"github.com/sirupsen/logrus"
)

func envGitSha(d *pb.Deployment) string {
	return d.Generation.SelectedCommitId
}

func envGitRef(d *pb.Deployment) string {
	return fmt.Sprintf("%s/%s", d.Generation.SelectedRemoteName, d.Generation.SelectedBranchName)
}

func envGitMessage(d *pb.Deployment) string {
	return strings.Trim(d.Generation.SelectedCommitMsg, "\n")
}

func envCominGeneration(d *pb.Deployment) string {
	return d.Generation.Uuid
}

func envCominHostname(d *pb.Deployment) string {
	return d.Generation.Hostname
}

func envCominStatus(d *pb.Deployment) string {
	return d.Status
}

func envCominErrorMessage(d *pb.Deployment) string {
	return d.ErrorMsg
}

// envCominOperation returns the deployment's operation. It falls back to
// "switch" so a caller reading this variable never sees an empty value,
// mirroring how consumers must already treat a missing COMIN_OPERATION
// (from a pre-fork comin) as "switch".
func envCominOperation(d *pb.Deployment) string {
	if d.Operation == "" {
		return "switch"
	}
	return d.Operation
}

func runPostDeploymentCommand(command string, d *pb.Deployment) (string, error) {

	cmd := exec.Command(command)

	cmd.Env = append(os.Environ(),
		"COMIN_GIT_SHA="+envGitSha(d),
		"COMIN_GIT_REF="+envGitRef(d),
		"COMIN_GIT_MSG="+envGitMessage(d),
		"COMIN_HOSTNAME="+envCominHostname(d),
		"COMIN_GENERATION="+envCominGeneration(d),
		"COMIN_STATUS="+envCominStatus(d),
		"COMIN_ERROR_MSG="+envCominErrorMessage(d),
		"COMIN_OPERATION="+envCominOperation(d),
	)

	output, err := cmd.CombinedOutput()
	outputString := string(output)
	if err != nil {
		return outputString, err
	}

	logrus.Debugf("cmd:[%s] output:[%s]", command, outputString)

	return outputString, nil
}
