package cmd

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	pb "github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	defer func() { os.Stdout = old }()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	f()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func reconcileState() *pb.State {
	return &pb.State{
		Builder:         &pb.Builder{Hostname: "my-machine"},
		Fetcher:         &pb.Fetcher{RepositoryStatus: &pb.RepositoryStatus{}},
		Deployer:        &pb.Deployer{},
		BuildConfirmer:  &pb.Confirmer{},
		DeployConfirmer: &pb.Confirmer{},
		Reconcile: &pb.Reconcile{
			State:   "waiting",
			Reason:  "refused: a-gate",
			Current: "/nix/store/old",
			Desired: &pb.ReconcileTarget{Commit: "abc123", Branch: "testing-robot-1", OutPath: "/nix/store/new"},
			Since:   timestamppb.Now(),
		},
	}
}

func TestLongStatusShowsTheReconcileState(t *testing.T) {
	out := captureStdout(t, func() { longStatus(reconcileState()) })
	assert.Contains(t, out, "State waiting since")
	assert.Contains(t, out, "Reason refused: a-gate")
	assert.Contains(t, out, "Desired commit abc123 on testing-robot-1 (/nix/store/new)")
	assert.Contains(t, out, "Current system /nix/store/old")
}

func TestOnelineStatusShowsTheReconcileState(t *testing.T) {
	out := captureStdout(t, func() { onelineStatus(reconcileState()) })
	assert.Contains(t, strings.TrimSpace(out), "waiting: refused: a-gate")

	noReason := reconcileState()
	noReason.Reconcile.Reason = ""
	out = captureStdout(t, func() { onelineStatus(noReason) })
	assert.Contains(t, strings.TrimSpace(out), "waiting")
	assert.NotContains(t, out, "refused")
}

func TestJSONStatusReconcileArtifact(t *testing.T) {
	out := captureStdout(t, func() { jsonStatus(reconcileState()) })
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	_, drift := v["drift"]
	assert.False(t, drift, "drift must be gone from the status artifact")
	rc, ok := v["reconcile"].(map[string]any)
	require.True(t, ok)
	keys := make([]string, 0, len(rc))
	for k := range rc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	assert.Equal(t, []string{"candidates", "current", "desired", "observed_at", "reason", "since", "state"}, keys)
}
