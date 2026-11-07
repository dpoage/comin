package manager

import (
	"context"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/reconcile"
	"github.com/nlewo/comin/internal/syncgate"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Option configures a Manager.
type Option func(*Manager)

// WithSyncGate asks c at the start of every pass and while an evaluation or
// a build runs.
func WithSyncGate(c syncgate.Command) Option {
	return func(m *Manager) { m.syncGate = c }
}

// manualRequest is the last switch-latest request; endedAt is zero until a
// decisive pass that started after it ran.
type manualRequest struct {
	requestedAt time.Time
	endedAt     time.Time
	state       string
	reason      string
}

func triggerOf(trigger string) syncgate.Trigger {
	if trigger == reconcile.TriggerManual {
		return syncgate.Manual
	}
	return syncgate.Poll
}

// requestManual records a request that reached comin now.
func (m *Manager) requestManual() {
	m.mu.Lock()
	m.manual = &manualRequest{requestedAt: time.Now(), state: reconcile.StateWaiting}
	m.mu.Unlock()
}

// manualPending reports whether a request still waits for its decisive pass.
func (m *Manager) manualPending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.manual != nil && m.manual.endedAt.IsZero()
}

// recordManual updates the request a manual pass served: the pass started
// after the request reached comin.
func (m *Manager) recordManual(started time.Time, trigger string, d reconcile.Decision) {
	if trigger != reconcile.TriggerManual {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.manual
	if r == nil || !r.endedAt.IsZero() || r.requestedAt.After(started) {
		return
	}
	r.state, r.reason = d.Status.State, d.Status.Reason
	if d.Decisive {
		r.endedAt = time.Now()
	}
}

func (m *Manager) manualStatus() *protobuf.ReconcileManual {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.manual
	if r == nil {
		return nil
	}
	out := &protobuf.ReconcileManual{
		RequestedAt: timestamppb.New(r.requestedAt),
		State:       r.state,
		Reason:      r.reason,
	}
	if !r.endedAt.IsZero() {
		out.EndedAt = timestamppb.New(r.endedAt)
	}
	return out
}

// passGate asks the gate at the start of a pass. A closed answer stops the
// running evaluation or build.
func (m *Manager) passGate(ctx context.Context, trigger string) *reconcile.SyncGateClosed {
	a := m.syncGate.Ask(ctx, syncgate.StagePass, triggerOf(trigger))
	if a.Open {
		m.gateReason = ""
		return nil
	}
	if a.Reason != m.gateReason {
		logrus.Infof("manager: sync gate closed: %s", a.Reason)
		m.gateReason = a.Reason
	}
	m.stopForGate(a.Reason)
	return &reconcile.SyncGateClosed{Reason: a.Reason}
}

// watchGate asks the gate every watchPeriod while an evaluation or a build
// runs, with the trigger of the request being served.
func (m *Manager) watchGate(ctx context.Context) {
	ticker := time.NewTicker(m.watchPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !m.Builder.IsEvaluating() && !m.Builder.IsBuilding() {
			continue
		}
		trigger := syncgate.Poll
		if m.manualPending() {
			trigger = syncgate.Manual
		}
		if a := m.syncGate.Ask(ctx, syncgate.StageWatch, trigger); !a.Open {
			m.stopForGate(a.Reason)
		}
	}
}

// stopForGate stops a running evaluation or build, serialized with the
// steps of a pass. The deployer is not touched.
func (m *Manager) stopForGate(reason string) {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	if m.Builder.StopForGate() {
		logrus.Infof("manager: the running evaluation or build is stopped: sync gate: %s", reason)
	}
}
