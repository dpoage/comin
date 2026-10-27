package deployer_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	require.NoError(t, err)
	return s
}

func TestSubmitDeploysTheCommitOfThePreviousDeployment(t *testing.T) {
	var mu sync.Mutex
	var outs []string
	deployFunc := func(_ context.Context, out, _ string) (bool, string, error) {
		mu.Lock()
		defer mu.Unlock()
		outs = append(outs, out)
		return false, "profile", nil
	}
	d := deployer.New(newStore(t), deployFunc, nil, "")
	d.Run(t.Context())

	g := &protobuf.Generation{Uuid: "g", SelectedCommitId: "commit-1", OutPath: "/out/1"}
	for range 2 {
		d.Submit(g, "switch")
		select {
		case <-d.DeploymentDoneCh:
		case <-time.After(5 * time.Second):
			t.Fatal("the generation of the previous deployment was not deployed again")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"/out/1", "/out/1"}, outs)
}

func TestPreActivation(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	deployFunc := func(_ context.Context, out, operation string) (bool, string, error) {
		record("deploy " + out + " " + operation)
		return false, "profile", nil
	}
	s := newStore(t)
	d := deployer.New(s, deployFunc, nil, "")
	var admit atomic.Bool
	d.SetPreActivation(func(_ context.Context, g *protobuf.Generation, operation string) bool {
		record("check " + g.OutPath + " " + operation)
		return admit.Load()
	})
	d.Run(t.Context())

	d.Submit(&protobuf.Generation{Uuid: "g1", OutPath: "/out/1"}, "switch")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	assert.False(t, d.IsDeploying())
	assert.Empty(t, s.DeploymentList(), "a refused submission is not recorded")

	admit.Store(true)
	d.Submit(&protobuf.Generation{Uuid: "g2", OutPath: "/out/2"}, "test")
	select {
	case dpl := <-d.DeploymentDoneCh:
		assert.Equal(t, "/out/2", dpl.Generation.OutPath)
		assert.Equal(t, "test", dpl.Operation)
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted generation was not deployed")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"check /out/1 switch", "check /out/2 test", "deploy /out/2 test"}, events)
}
