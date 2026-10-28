package manager

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedFailedRows commits n failed deployments of out, all ended before
// this process started, into the store the rig loads.
func seedFailedRows(t *testing.T, r *rig, commit, out string, n int) {
	t.Helper()
	bk := broker.New()
	bk.Start()
	st, err := store.New(bk, filepath.Join(r.stateDir, "store.json"), filepath.Join(r.stateDir, "gcroots"), 10, 10)
	require.NoError(t, err)
	past := time.Now().UTC().Add(-time.Hour)
	for i := range n {
		d := &protobuf.Deployment{
			Uuid:       uuid.New().String(),
			Generation: &protobuf.Generation{Uuid: fmt.Sprintf("seed-%d", i), SelectedCommitId: commit, OutPath: out},
			Operation:  "switch",
			Reason:     "manual",
			Status:     store.StatusToString(store.Failed),
			ErrorMsg:   "interrupted",
			StartedAt:  timestamppb.New(past),
			EndedAt:    timestamppb.New(past),
		}
		st.DeploymentInsertAndCommit(d)
	}
}

func TestARestartRetriesAnInterruptedDeploymentOnce(t *testing.T) {
	t.Run("a single interruption from before the restart is retried", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		head := r.mainHead()
		seedFailedRows(t, r, head, r.out(head), 1)
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(head))
		r.settle(10)
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("two interruptions from before the restart are not retried", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		head := r.mainHead()
		seedFailedRows(t, r, head, r.out(head), 2)
		r.start()
		require.Eventually(t, func() bool { return r.asksFor(r.out(head)) > 0 }, 15*time.Second, 25*time.Millisecond)
		r.settle(10)
		assert.Empty(t, r.exec.deployLog())
		assert.Equal(t, []string{"main"}, r.evalLabels())
	})

	t.Run("a retry that fails in this process is blocked", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true})
		head := r.mainHead()
		seedFailedRows(t, r, head, r.out(head), 1)
		r.exec.set(func(e *fakeExec) { e.failDeploy = true })
		r.start()
		r.waitDeploys(1)
		r.settle(10)
		assert.Len(t, r.exec.deployLog(), 1)
	})
}
