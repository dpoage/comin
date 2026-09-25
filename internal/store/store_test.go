package store

import (
	"testing"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
)

func TestDeploymentCommitAndLoad(t *testing.T) {
	tmp := t.TempDir()
	filename := tmp + "/state.json"
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, filename, tmp+"/gcroots", 2, 2)
	err := s.Commit()
	assert.Nil(t, err)

	s1, _ := New(bk, filename, tmp+"/gcroots", 2, 2)
	err = s1.Load()
	assert.Nil(t, err)
	assert.Equal(t, 0, len(s.data.Deployments))

	s.DeploymentInsert(&protobuf.Deployment{Uuid: "1", Operation: "switch"})
	_ = s.Commit()
	assert.Nil(t, err)

	s1, _ = New(bk, filename, tmp+"/gcroots", 2, 2)
	err = s1.Load()
	assert.Nil(t, err)
	assert.Equal(t, 1, len(s.data.Deployments))
}

func TestLastDeployment(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, "state.json", tmp+"/gcroots", 2, 2)
	ok, _ := s.LastDeployment()
	assert.False(t, ok)
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "1", Operation: "switch"})
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "2", Operation: "switch"})
	ok, last := s.LastDeployment()
	assert.True(t, ok)
	assert.Equal(t, "2", last.Uuid)
}

// C10: after a restart with exactly one stored deployment, that deployment
// is comin's current/previous deployment. Mutant: revert the fix (len(...) >
// 1 instead of > 0), which makes ok=false for exactly one deployment.
func TestLastDeploymentWithExactlyOne(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, "state.json", tmp+"/gcroots", 2, 2)
	ok, d := s.LastDeployment()
	assert.False(t, ok)
	assert.Nil(t, d)

	s.DeploymentInsert(&protobuf.Deployment{Uuid: "only", Operation: "switch"})
	ok, d = s.LastDeployment()
	assert.True(t, ok)
	assert.NotNil(t, d)
	assert.Equal(t, "only", d.Uuid)
}

func TestDeploymentInsert(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, "state.json", tmp+"/gcroots", 2, 2)
	var hasEvicted bool
	var evicted *protobuf.Deployment
	hasEvicted, _ = s.DeploymentInsert(&protobuf.Deployment{Uuid: "1", Operation: "switch"})
	assert.False(t, hasEvicted)
	hasEvicted, _ = s.DeploymentInsert(&protobuf.Deployment{Uuid: "2", Operation: "switch"})
	assert.False(t, hasEvicted)
	hasEvicted, evicted = s.DeploymentInsert(&protobuf.Deployment{Uuid: "3", Operation: "switch"})
	assert.True(t, hasEvicted)
	assert.Equal(t, "1", evicted.Uuid)
	assert.Equal(t, []string{"3", "2"}, uuids(s.DeploymentList()))

	hasEvicted, _ = s.DeploymentInsert(&protobuf.Deployment{Uuid: "4", Operation: "test"})
	assert.False(t, hasEvicted)
	hasEvicted, _ = s.DeploymentInsert(&protobuf.Deployment{Uuid: "5", Operation: "test"})
	assert.False(t, hasEvicted)
	hasEvicted, evicted = s.DeploymentInsert(&protobuf.Deployment{Uuid: "6", Operation: "test"})
	assert.True(t, hasEvicted)
	assert.Equal(t, "4", evicted.Uuid)
	assert.Equal(t, []string{"6", "5", "3", "2"}, uuids(s.DeploymentList()))

	hasEvicted, evicted = s.DeploymentInsert(&protobuf.Deployment{Uuid: "7", Operation: "switch"})
	assert.True(t, hasEvicted)
	assert.Equal(t, "2", evicted.Uuid)
	hasEvicted, evicted = s.DeploymentInsert(&protobuf.Deployment{Uuid: "8", Operation: "switch"})
	assert.True(t, hasEvicted)
	assert.Equal(t, "3", evicted.Uuid)
}

// The manager inserts every finished deployment again. When the next
// deployment already started, its row (appended by NewDeployment) is the
// last one; a store at capacity must evict the finished deployment's own
// row, not the one in flight, or the in-flight deployment's end is never
// recorded. Mutant: evict the last row of the kind.
func TestDeploymentInsertAtCapacityKeepsTheDeploymentInFlight(t *testing.T) {
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, "state.json", t.TempDir()+"/gcroots", 2, 2)
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "old", Operation: "switch", Status: StatusToString(Done)})
	a := s.NewDeployment(&protobuf.Generation{}, "switch", "")
	_, err := s.DeploymentStarted(a.Uuid)
	assert.NoError(t, err)
	a, err = s.DeploymentFinished(a.Uuid, nil, false, "")
	assert.NoError(t, err)
	b := s.NewDeployment(&protobuf.Generation{}, "switch", "")
	_, err = s.DeploymentStarted(b.Uuid)
	assert.NoError(t, err)

	hasEvicted, evicted := s.DeploymentInsert(a)
	assert.True(t, hasEvicted)
	assert.Equal(t, a.Uuid, evicted.Uuid)

	_, err = s.DeploymentFinished(b.Uuid, nil, false, "")
	assert.NoError(t, err, "the deployment in flight is still in the store")
	assert.Equal(t, []string{a.Uuid, "old", b.Uuid}, uuids(s.DeploymentList()))
}

func uuids(dpls []*protobuf.Deployment) []string {
	ids := make([]string, len(dpls))
	for i, d := range dpls {
		ids[i] = d.Uuid
	}
	return ids
}

func TestNewGeneration(t *testing.T) {
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, _ := New(bk, tmp+"/filename", tmp+"/gcroots", 2, 2)
	s.NewGeneration("hostname", "repositoryPath", "repositoryDir", "systemAttr", &protobuf.RepositoryStatus{})
}
