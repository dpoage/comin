package builder

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGateBuilder(t *testing.T) (*Builder, ExecutorMock) {
	t.Helper()
	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 1, 1)
	require.NoError(t, err)
	e := NewExecutorMock(false)
	return New(s, e, "", "", "", "my-machine", time.Minute, time.Minute), e
}

func waitFlag(t *testing.T, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 5*time.Second, 10*time.Millisecond)
}

func TestStopForGate(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		b, _ := newGateBuilder(t)
		assert.False(t, b.StopForGate())
		assert.False(t, b.gateStop.Load())
	})

	t.Run("a stopped evaluation records no failure", func(t *testing.T) {
		b, _ := newGateBuilder(t)
		require.NoError(t, b.Eval(t.Context(), repository.Candidate{}))
		waitFlag(t, b.IsEvaluating)
		uuid := b.GenerationUuid

		assert.True(t, b.StopForGate())
		assert.False(t, b.IsEvaluating())
		assert.False(t, b.gateStop.Load(), "the stop flag is left set")
		select {
		case got := <-b.EvaluationDone:
			assert.Equal(t, uuid, got)
		case <-time.After(5 * time.Second):
			t.Fatal("no EvaluationDone after the stop")
		}
		g, err := b.store.GenerationGet(uuid)
		require.NoError(t, err)
		assert.Equal(t, store.EvalInit.String(), g.EvalStatus)
		assert.Empty(t, g.EvalErr)
	})

	t.Run("a stopped build records no failure and can be built again", func(t *testing.T) {
		b, e := newGateBuilder(t)
		require.NoError(t, b.Eval(t.Context(), repository.Candidate{}))
		waitFlag(t, b.IsEvaluating)
		e.evalDone <- struct{}{}
		uuid := <-b.EvaluationDone
		require.NoError(t, b.build(t.Context(), uuid))
		waitFlag(t, b.IsBuilding)

		assert.True(t, b.StopForGate())
		assert.False(t, b.IsBuilding())
		select {
		case got := <-b.BuildDone:
			assert.Equal(t, uuid, got)
		case <-time.After(5 * time.Second):
			t.Fatal("no BuildDone after the stop")
		}
		g, err := b.store.GenerationGet(uuid)
		require.NoError(t, err)
		assert.Equal(t, store.Evaluated.String(), g.EvalStatus)
		assert.Equal(t, store.BuildInit.String(), g.BuildStatus)
		assert.Empty(t, g.BuildErr)
		assert.True(t, store.GenerationHasToBeBuilt(&g))

		require.NoError(t, b.build(t.Context(), uuid))
		waitFlag(t, b.IsBuilding)
		e.buildDone <- struct{}{}
		<-b.BuildDone
		g, err = b.store.GenerationGet(uuid)
		require.NoError(t, err)
		assert.Equal(t, store.Built.String(), g.BuildStatus)
	})

	t.Run("a stop that is not the gate's still records the failure", func(t *testing.T) {
		b, e := newGateBuilder(t)
		require.NoError(t, b.Eval(t.Context(), repository.Candidate{}))
		waitFlag(t, b.IsEvaluating)
		e.evalDone <- struct{}{}
		uuid := <-b.EvaluationDone
		require.NoError(t, b.build(t.Context(), uuid))
		waitFlag(t, b.IsBuilding)
		b.Stop()
		<-b.BuildDone
		g, err := b.store.GenerationGet(uuid)
		require.NoError(t, err)
		assert.Equal(t, store.BuildFailed.String(), g.BuildStatus)
		assert.Contains(t, g.BuildErr, "context canceled")
	})
}
