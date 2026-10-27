package manager

import (
	"context"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/executor"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
)

type switchLatestCall struct{ outPath, operation string }

func newSwitchLatestManager(t *testing.T, calls chan switchLatestCall) (*Manager, *store.Store) {
	t.Helper()
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	f := fetcher.NewFetcher(utils.NewRepositoryMock())
	b := builder.New(s, NewExecutorMock(""), "repoPath", "", "", "my-machine", 2*time.Second, 2*time.Second)
	d := deployer.New(s, func(_ context.Context, outPath, operation string) (bool, string, error) {
		calls <- switchLatestCall{outPath, operation}
		return false, "", nil
	}, nil, "")
	d.Run(t.Context())
	e, _ := executor.NewNixOSFlake()
	bc := NewConfirmer(bk, Without, 0, "")
	bc.Start()
	dc := NewConfirmer(bk, Without, 0, "")
	dc.Start()
	m := New(s, prometheus.New(), scheduler.New(), f, b, d, "", e, bc, dc, bk, emptyConfigurationOperations)
	return m, s
}

func TestSwitchDeploymentLatestWithoutPreviousDeployment(t *testing.T) {
	m, _ := newSwitchLatestManager(t, make(chan switchLatestCall, 1))
	assert.EqualError(t, m.SwitchDeploymentLatest(), "manager: no previous deployment")
}

// switch-latest re-deploys the last deployment's generation with "switch", whatever its operation was.
func TestSwitchDeploymentLatestDeploysTheLastDeploymentWithSwitch(t *testing.T) {
	calls := make(chan switchLatestCall, 1)
	m, s := newSwitchLatestManager(t, calls)
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "older", Operation: "test", Generation: &protobuf.Generation{Uuid: "g-older", OutPath: "/out/older"}})
	s.DeploymentInsert(&protobuf.Deployment{Uuid: "last", Operation: "test", Generation: &protobuf.Generation{Uuid: "g-last", OutPath: "/out/last"}})

	assert.NoError(t, m.SwitchDeploymentLatest())

	select {
	case c := <-calls:
		assert.Equal(t, switchLatestCall{"/out/last", "switch"}, c)
	case <-time.After(5 * time.Second):
		t.Fatal("the last deployment's generation was not deployed")
	}
}
