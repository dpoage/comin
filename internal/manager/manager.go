// The manager drives the components: it observes the fetcher, the
// builder, the confirmers, the store and the running system, asks the
// reconcile package for the next step and applies it (see pass.go).

package manager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/executor"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/profile"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/reconcile"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Manager struct {
	// The machine id of the current host. It is used to ensure
	// the optionnal machine-id found at evaluation time
	// corresponds to the machine-id of this host.
	machineId string

	stateRequestCh chan struct{}
	stateResultCh  chan *protobuf.State

	needToReboot bool

	prometheus      prometheus.Prometheus
	storage         *store.Store
	scheduler       scheduler.Scheduler
	Fetcher         *fetcher.Fetcher
	Builder         *builder.Builder
	deployer        *deployer.Deployer
	executor        executor.Executor
	BuildConfirmer  *Confirmer
	DeployConfirmer *Confirmer

	configurationOperations ConfigurationOperations

	// isSuspended is the suspension the passes read. Suspend sets it under
	// passMu, which a pass holds from observe through apply, before it
	// suspends the builder and the deployer.
	isSuspended atomic.Bool
	passMu      sync.Mutex

	broker *broker.Broker

	// preflight is the externally configured admission check.
	preflight preflight.Command

	// tick is the period of the passes that need no event.
	tick time.Duration
	// startedAt is this process's start, an input to failedLatest.
	startedAt time.Time
	exit      func(code int)
	// wakeCh requests a pass; triggerCh requests one with the given trigger.
	wakeCh    chan struct{}
	triggerCh chan string

	// cache, cacheSeq and retryEpoch belong to the pass goroutine. A failed
	// generation is evaluated again once retryEpoch moved past its own.
	cache      map[genKey]*cached
	cacheSeq   int
	retryEpoch uint64

	// mu guards the fields below.
	mu                sync.Mutex
	buildPending      string
	deployPending     string
	deployConfirmed   string
	inFlight          *submission
	reconcileSnapshot statusSnapshot
}

func New(s *store.Store,
	p prometheus.Prometheus,
	sched scheduler.Scheduler,
	fetcher *fetcher.Fetcher,
	builder *builder.Builder,
	deployer *deployer.Deployer,
	machineId string,
	executor executor.Executor,
	buildConfirmer *Confirmer,
	deployConfirmer *Confirmer,
	broker *broker.Broker,
	configurationOperations ConfigurationOperations,
	preflight preflight.Command,
) *Manager {

	m := &Manager{
		machineId:               machineId,
		stateRequestCh:          make(chan struct{}),
		stateResultCh:           make(chan *protobuf.State),
		prometheus:              p,
		storage:                 s,
		scheduler:               sched,
		Fetcher:                 fetcher,
		Builder:                 builder,
		deployer:                deployer,
		executor:                executor,
		BuildConfirmer:          buildConfirmer,
		DeployConfirmer:         deployConfirmer,
		broker:                  broker,
		configurationOperations: configurationOperations,
		preflight:               preflight,
		tick:                    30 * time.Second,
		startedAt:               time.Now(),
		exit:                    os.Exit,
		wakeCh:                  make(chan struct{}, 1),
		triggerCh:               make(chan string, 1),
		cache:                   map[genKey]*cached{},
		reconcileSnapshot: statusSnapshot{
			status: reconcile.Status{State: reconcile.StateWaiting, Reason: reconcile.ReasonNoCandidate},
		},
	}
	m.reconcileSnapshot.since = m.startedAt
	return m
}

func (m *Manager) GetState() *protobuf.State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m *Manager) toState() *protobuf.State {
	m.mu.Lock()
	snap := m.reconcileSnapshot
	m.mu.Unlock()
	rc := &protobuf.Reconcile{
		State:   snap.status.State,
		Current: snap.current,
		Reason:  snap.status.Reason,
		Since:   timestamppb.New(snap.since),
	}
	if !snap.observedAt.IsZero() {
		rc.ObservedAt = timestamppb.New(snap.observedAt)
	}
	for _, c := range snap.status.Candidates {
		rc.Candidates = append(rc.Candidates, &protobuf.ReconcileCandidate{
			Branch:  c.Branch,
			Kind:    c.Kind,
			Commit:  c.Commit,
			OutPath: c.OutPath,
			Outcome: c.Outcome,
			Reason:  c.Reason,
		})
	}
	if snap.status.Desired != nil {
		rc.Desired = &protobuf.ReconcileTarget{
			Commit:  snap.status.Desired.Commit,
			Branch:  snap.status.Desired.Branch,
			OutPath: snap.status.Desired.OutPath,
		}
	}
	return &protobuf.State{
		NeedToReboot:    wrapperspb.Bool(m.needToReboot),
		IsSuspended:     wrapperspb.Bool(m.isSuspended.Load()),
		Builder:         m.Builder.State(),
		Deployer:        m.deployer.State(),
		Fetcher:         m.Fetcher.GetState(),
		Store:           m.storage.GetState(),
		BuildConfirmer:  m.BuildConfirmer.status(),
		DeployConfirmer: m.DeployConfirmer.status(),
		Reconcile:       rc,
	}
}

// SwitchDeploymentLatest reconciles now: the passes that serve the request
// carry the manual trigger on their asks. It fails only when the repository
// offers no candidate, and returns without waiting for any deployment.
func (m *Manager) SwitchDeploymentLatest() error {
	if len(repository.Candidates(m.Fetcher.Latest())) == 0 {
		return fmt.Errorf("manager: %s", reconcile.ReasonNoCandidate)
	}
	select {
	case m.triggerCh <- reconcile.TriggerManual:
	default:
		// A request is already waiting for its decisive pass.
	}
	return nil
}

// Suspend marks the manager suspended for the passes before it suspends the
// builder, whose cancelled build wakes a pass, and the deployer.
func (m *Manager) Suspend() error {
	m.passMu.Lock()
	if m.isSuspended.Load() {
		m.passMu.Unlock()
		return fmt.Errorf("the manager is already suspended")
	}
	m.isSuspended.Store(true)
	m.passMu.Unlock()
	if err := m.Builder.Suspend(); err != nil {
		m.isSuspended.Store(false)
		return err
	}
	m.deployer.Suspend()
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Suspend_{Suspend: &protobuf.Event_Suspend{}}})
	return nil
}

// Resume resumes the builder, which restarts a build the suspension
// cancelled, and the deployer before the passes see the manager resumed.
func (m *Manager) Resume(ctx context.Context) error {
	if !m.isSuspended.Load() {
		return fmt.Errorf("the manager is not suspended")
	}
	if err := m.Builder.Resume(ctx); err != nil {
		return err
	}
	m.deployer.Resume()
	m.isSuspended.Store(false)
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Resume_{Resume: &protobuf.Event_Resume{}}})
	m.wake()
	return nil
}

// ConfigurationOperations is a map describing the operation associated
// to each remote/branch. It is a map looking such as:
// { origin: { main: switch, testing: test }, local { main: switch }
type ConfigurationOperations map[string](map[string]string)

func (m *Manager) getOperationFromConfigurationOperations(remote, branch string) (operation string) {
	operation = "test"
	branches, ok := m.configurationOperations[remote]
	if !ok {
		logrus.Errorf("manager: could not get the remote %s. Assuming 'test' operation", remote)
		return
	}
	operation, ok = branches[branch]
	if !ok {
		logrus.Errorf("manager: could not get the operation for the branch %s/%s. Assuming test operation", remote, branch)
		return
	}
	return
}

func (m *Manager) Run(ctx context.Context) {
	logrus.Infof("manager: starting with machineId=%s", m.machineId)
	lastDpl := m.deployer.State().Deployment
	if lastDpl != nil {
		m.needToReboot = m.executor.NeedToReboot(lastDpl.Generation.OutPath, lastDpl.Operation)
	}
	m.prometheus.SetHostInfo(m.needToReboot, m.isSuspended.Load())

	m.deployer.SetPreActivation(m.preActivation, m.abandoned)
	m.deployer.Run(ctx)
	go m.watch(ctx)
	go m.reconcileLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stateRequestCh:
			m.stateResultCh <- m.toState()
		case dpl := <-m.deployer.DeploymentDoneCh:
			m.prometheus.SetDeploymentInfo(dpl.Generation.SelectedCommitId, dpl.Status)
			getsEvicted, evicted := m.storage.DeploymentInsertAndCommit(dpl)

			// We remove the evicted deployment profile
			// path only if this profile path is not used
			// by any still alive other deployments.
			if getsEvicted && evicted.ProfilePath != "" {
				alive := false
				for _, d := range m.storage.DeploymentList() {
					if d.ProfilePath == evicted.ProfilePath {
						alive = true
					}
				}
				if !alive {
					_ = profile.RemoveProfilePath(evicted.ProfilePath)
				}
			}
			m.needToReboot = m.executor.NeedToReboot(dpl.Generation.OutPath, dpl.Operation)
			if m.needToReboot {
				e := &protobuf.Event_RebootRequired{Deployment: dpl}
				m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_RebootRequired_{RebootRequired: e}})
			}
			m.prometheus.SetHostInfo(m.needToReboot, m.isSuspended.Load())
			m.endSubmission()
			m.wake()
			if dpl.RestartComin.GetValue() {
				// TODO: stop contexts
				logrus.Infof("manager: comin needs to be restarted")
				logrus.Infof("manager: exiting comin to let the service manager restart it")
				m.exit(0)
			}
		}
	}
}
