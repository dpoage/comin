package manager

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// submissionCounter counts the generations the manager submits to the deploy confirmer.
type submissionCounter struct{ n atomic.Int64 }

func (c *submissionCounter) Levels() []logrus.Level { return []logrus.Level{logrus.InfoLevel} }

func (c *submissionCounter) Fire(e *logrus.Entry) error {
	if strings.HasPrefix(e.Message, "manager: the deployment of the generation ") {
		c.n.Add(1)
	}
	return nil
}

func countDeploySubmissions(t *testing.T) func() int {
	c := &submissionCounter{}
	logger := logrus.StandardLogger()
	old := logger.ReplaceHooks(logrus.LevelHooks{})
	logger.AddHook(c)
	t.Cleanup(func() { logger.ReplaceHooks(old) })
	return func() int { return int(c.n.Load()) }
}

func TestDeployConfirmationDuringAnAsk(t *testing.T) {
	t.Run("an auto confirmation is submitted once", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, autoDelay: 3 * time.Second, tick: time.Second})
		submissions := countDeploySubmissions(t)
		r.slowAsks(1500*time.Millisecond, 0)
		r.start()
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		r.settle(4)
		assert.Equal(t, 1, submissions(), "deploy confirmer submissions")
		assert.Len(t, r.exec.deployLog(), 1)
	})

	t.Run("a manual confirmation leaves nothing awaiting confirmation", func(t *testing.T) {
		r := newRig(t, rigOpts{preflight: true, manual: true})
		submissions := countDeploySubmissions(t)
		r.slowAsks(1500*time.Millisecond, 0)
		r.start()
		uuid := r.waitPending("")
		n := len(r.asks())
		require.Eventually(t, func() bool { return len(r.asks()) > n }, 15*time.Second, 10*time.Millisecond, "no ask started")
		r.m.DeployConfirmer.Confirm(uuid)
		r.waitDeploys(1)
		r.waitCurrent(r.out(r.mainHead()))
		time.Sleep(3 * time.Second)
		assert.Equal(t, 1, submissions(), "deploy confirmer submissions")
		assert.Empty(t, r.m.DeployConfirmer.status().Submitted, "the deployed generation is shown as awaiting confirmation")
		r.m.mu.Lock()
		pending := r.m.deployPending
		r.m.mu.Unlock()
		assert.Empty(t, pending)
	})
}
