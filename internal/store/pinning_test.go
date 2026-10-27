package store

import (
	"errors"
	"testing"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	bk := broker.New()
	bk.Start()
	s, err := New(bk, t.TempDir()+"/state.json", t.TempDir()+"/gcroots", 10, 10)
	assert.NoError(t, err)
	return s
}

// A deploy func error is stored as failed, not done.
func TestDeploymentFinishedWithErrorIsFailed(t *testing.T) {
	s := newTestStore(t)
	dpl := s.NewDeployment(&protobuf.Generation{}, "switch", "")
	_, err := s.DeploymentStarted(dpl.Uuid)
	assert.NoError(t, err)

	finished, err := s.DeploymentFinished(dpl.Uuid, errors.New("switch failed"), false, "")
	assert.NoError(t, err)
	assert.Equal(t, StatusToString(Failed), finished.Status)
	assert.Equal(t, "switch failed", finished.ErrorMsg)
	assert.NotNil(t, finished.EndedAt)

	stored := s.DeploymentList()
	assert.Len(t, stored, 1)
	assert.Equal(t, StatusToString(Failed), stored[0].Status)
	assert.Equal(t, "switch failed", stored[0].ErrorMsg)
}

func TestDeploymentFinishedWithoutErrorIsDone(t *testing.T) {
	s := newTestStore(t)
	dpl := s.NewDeployment(&protobuf.Generation{}, "switch", "")
	_, err := s.DeploymentStarted(dpl.Uuid)
	assert.NoError(t, err)

	finished, err := s.DeploymentFinished(dpl.Uuid, nil, true, "profile")
	assert.NoError(t, err)
	assert.Equal(t, StatusToString(Done), finished.Status)
	assert.Equal(t, "profile", finished.ProfilePath)
	assert.True(t, finished.RestartComin.GetValue())
	assert.Equal(t, StatusToString(Done), s.DeploymentList()[0].Status)
}

// The values NewDeployment, DeploymentStarted and DeploymentFinished return are copies.
func TestDeploymentTransitionsReturnCopies(t *testing.T) {
	s := newTestStore(t)

	created := s.NewDeployment(&protobuf.Generation{OutPath: "out"}, "switch", "")
	created.Operation = "mutated"
	created.Status = "mutated"
	created.Generation.OutPath = "mutated"
	stored := s.DeploymentList()[0]
	assert.Equal(t, "switch", stored.Operation)
	assert.Equal(t, StatusToString(Init), stored.Status)
	assert.Equal(t, "out", stored.Generation.OutPath)

	started, err := s.DeploymentStarted(created.Uuid)
	assert.NoError(t, err)
	started.Status = "mutated"
	started.Operation = "mutated"
	assert.Equal(t, StatusToString(Running), s.DeploymentList()[0].Status)
	assert.Equal(t, "switch", s.DeploymentList()[0].Operation)

	finished, err := s.DeploymentFinished(created.Uuid, nil, false, "profile")
	assert.NoError(t, err)
	finished.Status = "mutated"
	finished.ProfilePath = "mutated"
	assert.Equal(t, StatusToString(Done), s.DeploymentList()[0].Status)
	assert.Equal(t, "profile", s.DeploymentList()[0].ProfilePath)
}

// DeploymentList returns copies.
func TestDeploymentListReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "1", Operation: "switch"})

	list := s.DeploymentList()
	list[0].Uuid = "mutated"
	list[0].Operation = "mutated"
	list[0] = nil

	assert.Equal(t, []string{"1"}, uuids(s.DeploymentList()))
	assert.Equal(t, "switch", s.DeploymentList()[0].Operation)
}

// GetState returns a copy.
func TestGetStateReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "1", Operation: "switch"})
	s.NewGeneration("host", "path", "dir", "attr", &protobuf.RepositoryStatus{})

	state := s.GetState()
	state.Deployments[0].Uuid = "mutated"
	state.Deployments = nil
	state.Generations[0].Hostname = "mutated"
	state.Generations = nil

	assert.Equal(t, []string{"1"}, uuids(s.DeploymentList()))
	assert.Len(t, s.GetState().Generations, 1)
	assert.Equal(t, "host", s.GetState().Generations[0].Hostname)
}

// NewGeneration returns a deep copy.
func TestNewGenerationReturnsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	rs := &protobuf.RepositoryStatus{SelectedBranchIsTesting: wrapperspb.Bool(false)}
	g := s.NewGeneration("host", "path", "dir", "attr", rs)
	g.SelectedBranchIsTesting.Value = true
	g.Hostname = "mutated"

	stored := s.GetState().Generations
	assert.Len(t, stored, 1)
	assert.False(t, stored[0].SelectedBranchIsTesting.GetValue())
	assert.Equal(t, "host", stored[0].Hostname)
}
