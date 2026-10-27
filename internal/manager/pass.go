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

const (
	triggerPoll = "poll"
	// maxCached bounds the generation cache; the oldest entries go first.
	maxCached = 256
)

// genKey identifies the generation of a candidate: at most one is evaluated
// per commit and testing flag.
type genKey struct {
	commit  string
	testing bool
}

// cached is the generation evaluated for a genKey. The store only keeps the
// generations the builder still references, so gen holds the last value read.
type cached struct {
	uuid  string
	gen   *protobuf.Generation
	stage reconcile.Stage
	err   string
	seq   int
}

// submission is the deployment handed to the deployer and not yet finished.
type submission struct {
	uuid    string
	outPath string
	trigger string
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

// reconcileLoop runs a pass at start, then on every wake, trigger and tick.
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	trigger := triggerPoll
	for {
		m.pass(ctx, trigger)
		trigger = triggerPoll
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wakeCh:
		case trigger = <-m.triggerCh:
		}
	}
}

// watch is the only reader of the fetcher, builder and confirmer channels.
// It records facts and wakes the pass; it never asks the preflight.
func (m *Manager) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.Fetcher.Fetched():
			m.wake()
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
func (m *Manager) pass(ctx context.Context, trigger string) {
	verdicts := map[genKey]preflight.Verdict{}
	for ctx.Err() == nil {
		m.passMu.Lock()
		obs, entries := m.observe(trigger, verdicts)
		d := reconcile.Decide(obs)
		if d.Action != reconcile.Ask {
			m.apply(ctx, obs, entries, d)
			m.passMu.Unlock()
			return
		}
		m.passMu.Unlock()
		c := obs.Candidates[d.Target]
		v := preflight.Ask(ctx, m.preflight, askEnv(entries[d.Target].gen, m.operation(c.Candidate), obs.Current, obs.Trigger))
		logrus.Infof("manager: preflight for %s: admitted=%t %s", c.CommitId, v.Admitted, v.Reason)
		verdicts[genKey{c.CommitId, c.Testing}] = v
	}
}

// observe gathers one Observation. While suspended it walks no candidate,
// so a build the suspension cancelled is never read.
func (m *Manager) observe(trigger string, verdicts map[genKey]preflight.Verdict) (reconcile.Observation, []*cached) {
	current, currentErr := m.currentSystem()
	m.mu.Lock()
	obs := reconcile.Observation{
		Current:        current,
		CurrentErr:     currentErr,
		InFlight:       m.inFlight != nil,
		DeployConfirm:  reconcile.DeployConfirm{Pending: m.deployPending, Confirmed: m.deployConfirmed},
		LastActivation: m.lastActivation,
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

	candidates := repository.Candidates(m.Fetcher.Latest())
	entries := make([]*cached, len(candidates))
	for i, c := range candidates {
		key := genKey{c.CommitId, c.Testing}
		rc := reconcile.Candidate{Candidate: c}
		if v, ok := verdicts[key]; ok {
			rc.Verdict = &v
		}
		if e := m.cache[key]; e != nil && m.refresh(key, e) {
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
		m.inFlight = &submission{uuid: c.GenerationUuid, outPath: c.OutPath, trigger: obs.Trigger}
		m.deployConfirmed = ""
		m.mu.Unlock()
		m.deployer.Submit(proto.CloneOf(entries[d.Target].gen), m.operation(c.Candidate))
	}
}

// eval requests the evaluation of c. The builder holds one generation, so
// the entries it is about to preempt are forgotten.
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
	trigger := triggerPoll
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
	m.mu.Lock()
	m.lastActivation = &reconcile.Activation{OutPath: g.OutPath, Verdict: v}
	m.mu.Unlock()
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
