package manager

import (
	"context"
	"strings"
	"time"

	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/reconcile"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// maxCached bounds the generation cache; the oldest entries go first.
const maxCached = 256

// genKey identifies the generation of a candidate: at most one is evaluated
// per commit and testing flag, and a failed one once more per retryEpoch.
type genKey struct {
	commit  string
	testing bool
}

// cached is the generation evaluated for a genKey; epoch is the retryEpoch
// recorded when its failure landed. The store only keeps the generations
// the builder still references, so gen holds the last value read.
type cached struct {
	uuid  string
	gen   *protobuf.Generation
	stage reconcile.Stage
	err   string
	seq   int
	epoch uint64
}

// submission is the deployment handed to the deployer and not yet finished.
type submission struct {
	uuid    string
	target  reconcile.Target
	trigger string
}

// statusSnapshot is the last pass's status with the running system it was
// computed against, when its state value was first observed and when the
// pass started (zero before the first pass).
type statusSnapshot struct {
	status     reconcile.Status
	current    string
	since      time.Time
	observedAt time.Time
}

func (m *Manager) wake() {
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

func (m *Manager) endSubmission() {
	m.mu.Lock()
	m.inFlight = nil
	m.mu.Unlock()
}

// abandoned is the deployer's report that it dropped g because it got
// suspended during the pre-activation ask. The confirmation of g stands:
// the pass after Resume decides again.
func (m *Manager) abandoned(g *protobuf.Generation) {
	m.mu.Lock()
	if m.inFlight != nil && m.inFlight.uuid == g.Uuid {
		m.deployConfirmed = g.Uuid
	}
	m.inFlight = nil
	m.mu.Unlock()
	m.wake()
}

// reconcileLoop runs a pass at start, then on every wake, fetch, trigger and
// tick; passes carry the manual trigger until one is decisive. It alone reads
// the fetcher's channel; each signal or request moves retryEpoch.
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	manual := false
	for {
		// A request that reached comin before the stamp is served by this pass.
		started := time.Now()
		select {
		case <-m.triggerCh:
			manual = true
			m.retryEpoch++
		default:
		}
		select {
		case <-m.Fetcher.Fetched():
			m.retryEpoch++
		default:
		}
		trigger := reconcile.TriggerPoll
		if manual {
			trigger = reconcile.TriggerManual
		}
		d := m.pass(ctx, started, trigger)
		manual = manual && !d.Decisive
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wakeCh:
		case <-m.Fetcher.Fetched():
			m.retryEpoch++
		case <-m.triggerCh:
			manual = true
			m.retryEpoch++
		}
	}
}

// watch is the only reader of the builder and confirmer channels.
// It records facts and wakes the pass; it never asks the preflight.
func (m *Manager) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.Builder.EvaluationDone:
			m.wake()
		case <-m.Builder.BuildDone:
			m.wake()
		case uuid := <-m.BuildConfirmer.confirmed:
			m.mu.Lock()
			if m.buildPending == uuid {
				m.buildPending = ""
			}
			m.mu.Unlock()
			m.Builder.SubmitBuild(ctx, uuid)
		case uuid := <-m.DeployConfirmer.confirmed:
			m.mu.Lock()
			if m.deployPending == uuid {
				m.deployPending = ""
			}
			m.deployConfirmed = uuid
			m.mu.Unlock()
			m.wake()
		}
	}
}

// pass decides and applies steps until one needs an external event. The
// verdicts of its asks live for the pass only. A step holds passMu from
// observe through apply; asks run without it.
// started precedes every read of the pass's inputs.
func (m *Manager) pass(ctx context.Context, started time.Time, trigger string) reconcile.Decision {
	verdicts := map[genKey]preflight.Verdict{}
	for ctx.Err() == nil {
		m.passMu.Lock()
		obs, entries := m.observe(trigger, verdicts)
		d := reconcile.Decide(obs)
		if d.Action != reconcile.Ask {
			m.apply(ctx, obs, entries, d)
			m.recordStatus(started, obs.Current, d.Status)
			m.passMu.Unlock()
			return d
		}
		m.passMu.Unlock()
		c := obs.Candidates[d.Target]
		v := preflight.Ask(ctx, m.preflight, askEnv(entries[d.Target].gen, m.operation(c.Candidate), obs.Current, obs.Trigger))
		logrus.Infof("manager: preflight for %s: admitted=%t %s", c.CommitId, v.Admitted, v.Reason)
		verdicts[genKey{c.CommitId, c.Testing}] = v
	}
	return reconcile.Decision{}
}

// recordStatus keeps the last pass's status and its start; since advances
// only when the state value changed.
func (m *Manager) recordStatus(started time.Time, current string, st reconcile.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.State != m.reconcileSnapshot.status.State {
		m.reconcileSnapshot = statusSnapshot{status: st, current: current, since: time.Now(), observedAt: started}
		return
	}
	m.reconcileSnapshot.status = st
	m.reconcileSnapshot.current = current
	m.reconcileSnapshot.observedAt = started
}

// observe gathers one Observation. While suspended it walks no candidate,
// so a build the suspension cancelled is never read.
func (m *Manager) observe(trigger string, verdicts map[genKey]preflight.Verdict) (reconcile.Observation, []*cached) {
	current, currentErr := m.currentSystem()
	m.mu.Lock()
	var inFlightTarget *reconcile.Target
	if m.inFlight != nil {
		t := m.inFlight.target
		inFlightTarget = &t
	}
	obs := reconcile.Observation{
		Current:        current,
		CurrentErr:     currentErr,
		InFlight:       m.inFlight != nil,
		InFlightTarget: inFlightTarget,
		DeployConfirm:  reconcile.DeployConfirm{Pending: m.deployPending, Confirmed: m.deployConfirmed},
	}
	buildPending := m.buildPending
	m.mu.Unlock()
	obs.InFlight = obs.InFlight || m.deployer.IsDeploying()
	obs.Suspended = m.isSuspended.Load()
	obs.ProcessStartedAt = m.startedAt
	obs.AsksRun = m.preflight.Path != ""
	obs.Trigger = trigger
	obs.Deployments = m.storage.DeploymentList()
	if obs.Suspended {
		return obs, nil
	}

	epoch := m.retryEpoch
	candidates := repository.Candidates(m.Fetcher.Latest())
	entries := make([]*cached, len(candidates))
	for i, c := range candidates {
		key := genKey{c.CommitId, c.Testing}
		rc := reconcile.Candidate{Candidate: c}
		if v, ok := verdicts[key]; ok {
			rc.Verdict = &v
		}
		// A due re-attempt waits while another candidate holds the builder.
		if e := m.cache[key]; e != nil && m.refresh(key, e) && (!e.retryDue(epoch) || m.builderHeld(key)) {
			entries[i] = e
			rc.Stage, rc.Err, rc.GenerationUuid, rc.OutPath = e.stage, e.err, e.uuid, e.gen.OutPath
			rc.BuildSubmitted = buildPending == e.uuid
			if e.stage == reconcile.Built {
				rc.Realized = m.executor.IsStorePathExist(e.gen.OutPath)
			}
		}
		obs.Candidates = append(obs.Candidates, rc)
	}
	return obs, entries
}

// refresh reads the generation of e from the store. It forgets e, and
// returns false, when the store lost a generation that was not finished.
func (m *Manager) refresh(key genKey, e *cached) bool {
	g, err := m.storage.GenerationGet(e.uuid)
	if err == nil {
		e.gen = &g
	} else if e.gen == nil || !finished(e.stage) {
		delete(m.cache, key)
		return false
	}
	stage, errMsg := m.stageOf(e.gen)
	if stage == reconcile.MachineIdMismatch && e.stage != stage {
		logrus.Infof("manager: the comin.machineId %s is not the host machine-id %s", e.gen.MachineId, m.machineId)
	}
	if (stage == reconcile.EvalFailed || stage == reconcile.BuildFailed) && e.stage != stage {
		// The attempt covers every fetch completing before its failure lands.
		e.epoch = m.retryEpoch
	}
	e.stage, e.err = stage, errMsg
	return true
}

func (m *Manager) stageOf(g *protobuf.Generation) (reconcile.Stage, string) {
	switch g.EvalStatus {
	case store.EvalFailed.String():
		return reconcile.EvalFailed, g.EvalErr
	case store.Evaluated.String():
	default:
		return reconcile.Evaluating, ""
	}
	if g.MachineId != "" && m.machineId != g.MachineId {
		return reconcile.MachineIdMismatch, ""
	}
	switch g.BuildStatus {
	case store.Built.String():
		return reconcile.Built, ""
	case store.BuildFailed.String():
		return reconcile.BuildFailed, g.BuildErr
	case store.Building.String():
		return reconcile.Building, ""
	}
	return reconcile.AwaitingBuild, ""
}

// retryDue reports whether e failed in an epoch that epoch has moved past;
// the pass then sees the candidate as never evaluated.
func (e *cached) retryDue(epoch uint64) bool {
	return (e.stage == reconcile.EvalFailed || e.stage == reconcile.BuildFailed) && e.epoch < epoch
}

// builderHeld reports whether a candidate other than key still holds the
// single-slot builder: evaluating, building, or awaiting its build.
func (m *Manager) builderHeld(key genKey) bool {
	for k, e := range m.cache {
		if k != key && !finished(e.stage) {
			return true
		}
	}
	return false
}

func finished(s reconcile.Stage) bool {
	switch s {
	case reconcile.Built, reconcile.EvalFailed, reconcile.BuildFailed, reconcile.MachineIdMismatch:
		return true
	}
	return false
}

func (m *Manager) apply(ctx context.Context, obs reconcile.Observation, entries []*cached, d reconcile.Decision) {
	if d.Action == reconcile.Wait {
		logrus.Debugf("manager: nothing to do")
		return
	}
	c := obs.Candidates[d.Target]
	switch d.Action {
	case reconcile.Eval:
		m.eval(ctx, c.Candidate)
	case reconcile.SubmitBuild:
		logrus.Infof("manager: the build of the generation %s is submitted", c.GenerationUuid)
		m.mu.Lock()
		m.buildPending = c.GenerationUuid
		m.mu.Unlock()
		m.BuildConfirmer.Submit(c.GenerationUuid)
	case reconcile.SubmitDeploy:
		logrus.Infof("manager: the deployment of the generation %s is submitted", c.GenerationUuid)
		m.mu.Lock()
		m.deployPending = c.GenerationUuid
		m.deployConfirmed = ""
		m.mu.Unlock()
		m.DeployConfirmer.Submit(c.GenerationUuid)
	case reconcile.Deploy:
		m.mu.Lock()
		m.inFlight = &submission{uuid: c.GenerationUuid, target: reconcile.TargetOf(c), trigger: obs.Trigger}
		m.deployConfirmed = ""
		m.mu.Unlock()
		m.deployer.Submit(proto.CloneOf(entries[d.Target].gen), m.operation(c.Candidate))
	}
}

// eval requests the evaluation of c. The builder holds one generation, so
// the entries it is about to preempt are forgotten; a re-attempt runs only
// once that set is empty, so it preempts nothing.
func (m *Manager) eval(ctx context.Context, c repository.Candidate) {
	for k, e := range m.cache {
		if !finished(e.stage) {
			delete(m.cache, k)
		}
	}
	logrus.Infof("manager: a generation is evaluating for commit %s", c.CommitId)
	if err := m.Builder.Eval(ctx, c); err != nil {
		logrus.Error(err)
		return
	}
	m.cacheSeq++
	m.cache[genKey{c.CommitId, c.Testing}] = &cached{uuid: m.Builder.State().GenerationUuid, seq: m.cacheSeq}
	for len(m.cache) > maxCached {
		var oldest genKey
		first := true
		for k, e := range m.cache {
			if first || e.seq < m.cache[oldest].seq {
				oldest, first = k, false
			}
		}
		delete(m.cache, oldest)
	}
}

// preActivation is the deployer's last check before an activation: the
// generation must be realized and the preflight must admit it.
func (m *Manager) preActivation(ctx context.Context, g *protobuf.Generation, operation string) bool {
	trigger := reconcile.TriggerPoll
	m.mu.Lock()
	if m.inFlight != nil {
		trigger = m.inFlight.trigger
	}
	m.mu.Unlock()
	if m.preflight.Path != "" && !m.executor.IsStorePathExist(g.OutPath) {
		logrus.Infof("manager: the out path %s of the generation %s is not in the store: not activated", g.OutPath, g.Uuid)
		m.endSubmission()
		m.wake()
		return false
	}
	current, _ := m.currentSystem()
	v := preflight.Ask(ctx, m.preflight, askEnv(g, operation, current, trigger))
	if !v.Admitted {
		logrus.Infof("manager: the activation of %s is refused: %s", g.OutPath, v.Reason)
		m.endSubmission()
		m.wake()
	}
	return v.Admitted
}

// currentSystem returns the running system and, when it is unknown, "" and
// its cause.
func (m *Manager) currentSystem() (current, cause string) {
	current, err := m.executor.CurrentSystem()
	if err != nil {
		return "", err.Error()
	}
	return current, ""
}

func (m *Manager) operation(c repository.Candidate) string {
	return m.getOperationFromConfigurationOperations(c.Remote, c.Branch)
}

func askEnv(g *protobuf.Generation, operation, current, trigger string) []string {
	branch := "main"
	if g.SelectedBranchIsTesting.GetValue() {
		branch = "testing"
	}
	return []string{
		"COMIN_OUT_PATH=" + g.OutPath,
		"COMIN_GIT_SHA=" + g.SelectedCommitId,
		"COMIN_GIT_REF=" + g.SelectedRemoteName + "/" + g.SelectedBranchName,
		"COMIN_GIT_MSG=" + strings.TrimRight(g.SelectedCommitMsg, "\n"),
		"COMIN_BRANCH=" + branch,
		"COMIN_OPERATION=" + operation,
		"COMIN_CURRENT_SYSTEM=" + current,
		"COMIN_TRIGGER=" + trigger,
		"COMIN_HOSTNAME=" + g.Hostname,
	}
}
