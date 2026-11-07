package manager

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/reconcile"
	"github.com/stretchr/testify/assert"
)

func TestRecordManual(t *testing.T) {
	started := time.Unix(1000, 0)
	decisive := reconcile.Decision{Decisive: true, Status: reconcile.Status{State: reconcile.StateWaiting, Reason: "sync gate: x"}}
	pending := reconcile.Decision{Status: reconcile.Status{State: reconcile.StateWaiting, Reason: "building c"}}
	tests := []struct {
		name      string
		requested time.Time
		trigger   string
		d         reconcile.Decision
		ended     bool
		state     string
		reason    string
	}{
		{"served by the pass that started after it", started.Add(-time.Nanosecond), reconcile.TriggerManual, decisive, true, "waiting", "sync gate: x"},
		{"served by the pass that started at the same instant", started, reconcile.TriggerManual, decisive, true, "waiting", "sync gate: x"},
		{"a request after the pass started is left to the next pass", started.Add(time.Nanosecond), reconcile.TriggerManual, decisive, false, "converged", "old"},
		{"a pass that is not decisive records its status", started.Add(-time.Second), reconcile.TriggerManual, pending, false, "waiting", "building c"},
		{"a poll pass serves no request", started.Add(-time.Second), reconcile.TriggerPoll, decisive, false, "converged", "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{manual: &manualRequest{requestedAt: tt.requested, state: "converged", reason: "old"}}
			m.recordManual(started, tt.trigger, tt.d)
			got := m.manual
			assert.Equal(t, tt.ended, !got.endedAt.IsZero())
			assert.Equal(t, tt.state, got.state)
			assert.Equal(t, tt.reason, got.reason)
			assert.Equal(t, !tt.ended, m.manualPending())
		})
	}

	t.Run("an ended request is not updated again", func(t *testing.T) {
		ended := started.Add(-time.Minute)
		m := &Manager{manual: &manualRequest{requestedAt: started.Add(-time.Hour), endedAt: ended, state: "converged", reason: "old"}}
		m.recordManual(started, reconcile.TriggerManual, decisive)
		assert.Equal(t, ended, m.manual.endedAt)
		assert.Equal(t, "converged", m.manual.state)
		assert.False(t, m.manualPending())
	})

	t.Run("no request", func(t *testing.T) {
		m := &Manager{}
		m.recordManual(started, reconcile.TriggerManual, decisive)
		assert.Nil(t, m.manual)
		assert.False(t, m.manualPending())
		assert.Nil(t, m.manualStatus())
	})
}
