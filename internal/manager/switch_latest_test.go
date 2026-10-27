package manager

import (
	"context"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSwitchLatestManager(t *testing.T) (*Manager, *store.Store, *utils.RepositoryMock) {
	t.Helper()
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	r := utils.NewRepositoryMock()
	f := fetcher.NewFetcher(r)
	b := builder.New(s, NewExecutorMock(""), "repoPath", "", "", "my-machine", 2*time.Second, 2*time.Second)
	d := deployer.New(s, func(_ context.Context, _, _ string) (bool, string, error) {
		t.Error("switch-latest must not deploy by itself")
		return false, "", nil
	}, nil, "")
	d.Run(t.Context())
	bc := NewConfirmer(bk, Without, 0, "")
	bc.Start()
	dc := NewConfirmer(bk, Without, 0, "")
	dc.Start()
	m := New(s, prometheus.New(), scheduler.New(), f, b, d, "", NewExecutorMock(""), bc, dc, bk, emptyConfigurationOperations, preflight.Command{})
	return m, s, r
}

func TestSwitchDeploymentLatestErrorsWithoutACandidate(t *testing.T) {
	m, s, _ := newSwitchLatestManager(t)
	// A stored deployment is not a candidate: only the repository is.
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "last", Operation: "switch", Generation: &protobuf.Generation{Uuid: "g", OutPath: "/out/last"}})
	assert.EqualError(t, m.SwitchDeploymentLatest(), "manager: no candidate")
}

func TestSwitchDeploymentLatestReturnsNilWithACandidate(t *testing.T) {
	m, _, r := newSwitchLatestManager(t)
	m.Fetcher.Start(t.Context())
	m.Fetcher.TriggerFetch([]string{"remote"})
	r.RsCh <- rsAtMain("id-1")
	require.Eventually(t, func() bool { return m.Fetcher.Latest() != nil }, 5*time.Second, 50*time.Millisecond)

	assert.NoError(t, m.SwitchDeploymentLatest())
	assert.Nil(t, m.deployer.State().Deployment, "switch-latest does not deploy by itself")
}
