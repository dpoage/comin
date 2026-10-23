package cmd

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// capture returns what f writes to os.Stdout.
func capture(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() { os.Stdout = stdout }()
		f()
	}()
	require.NoError(t, w.Close())
	return <-done
}

func statusFixture() *pb.State {
	return &pb.State{
		NeedToReboot:    wrapperspb.Bool(false),
		IsSuspended:     wrapperspb.Bool(false),
		Builder:         &pb.Builder{Hostname: "r1"},
		Deployer:        &pb.Deployer{},
		Fetcher:         &pb.Fetcher{RepositoryStatus: &pb.RepositoryStatus{}},
		BuildConfirmer:  &pb.Confirmer{},
		DeployConfirmer: &pb.Confirmer{},
	}
}

func preDeployGen(uuid, commit string) *pb.Generation {
	return &pb.Generation{Uuid: uuid, SelectedCommitId: commit, SelectedRemoteName: "origin", SelectedBranchName: "main"}
}

// The output of longStatus before the pre-deploy section existed.
const baseLongStatus = `Status of the machine r1
  Fetcher
  Builder
    No build available
  Deployer
    No deployment yet
`

func TestLongStatusWithoutPreDeployIsUnchanged(t *testing.T) {
	for name, pd := range map[string]*pb.PreDeploy{"absent": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			s := statusFixture()
			s.PreDeploy = pd
			assert.Equal(t, baseLongStatus, capture(t, func() { longStatus(s) }))
		})
	}
}

func TestLongStatusPreDeploySection(t *testing.T) {
	s := statusFixture()
	s.PreDeploy = &pb.PreDeploy{
		Running: &pb.PreDeployEntry{
			Generation: preDeployGen("gen-1", "abc123"), Operation: "switch",
			StartedAt: timestamppb.New(time.Now().Add(-2 * time.Hour)),
		},
		Pending: &pb.PreDeployEntry{Generation: preDeployGen("gen-2", "def456"), Operation: "switch"},
	}
	out := capture(t, func() { longStatus(s) })
	assert.Contains(t, out, "  Pre-deploy\n")
	assert.Contains(t, out, "    Running pre-deployment command for generation gen-1 (commit abc123 from origin/main) since 2 hours ago\n")
	assert.Contains(t, out, "    Waiting: generation gen-2 (commit def456 from origin/main)\n")

	s.PreDeploy.Pending = nil
	out = capture(t, func() { longStatus(s) })
	assert.Contains(t, out, "Running pre-deployment command")
	assert.NotContains(t, out, "Waiting")

	s.PreDeploy.Running, s.PreDeploy.Pending = nil, &pb.PreDeployEntry{Generation: preDeployGen("gen-2", "def456")}
	out = capture(t, func() { longStatus(s) })
	assert.NotContains(t, out, "Running pre-deployment")
	assert.Contains(t, out, "    Waiting: generation gen-2 (commit def456 from origin/main)\n")
	assert.True(t, strings.Index(out, "Pre-deploy") < strings.Index(out, "Deployer"), "the section precedes the deployer")
}

func TestJSONStatusCarriesPreDeploy(t *testing.T) {
	s := statusFixture()
	s.PreDeploy = &pb.PreDeploy{}
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(capture(t, func() { jsonStatus(s) })), &got))
	assert.Equal(t, map[string]any{"running": nil, "pending": nil}, got["pre_deploy"])

	s.PreDeploy.Running = &pb.PreDeployEntry{Generation: preDeployGen("gen-1", "abc123"), StartedAt: timestamppb.Now()}
	require.NoError(t, json.Unmarshal([]byte(capture(t, func() { jsonStatus(s) })), &got))
	running := got["pre_deploy"].(map[string]any)["running"].(map[string]any)
	assert.Equal(t, "gen-1", running["generation"].(map[string]any)["uuid"])
	assert.NotNil(t, running["started_at"])
}
