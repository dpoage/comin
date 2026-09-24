// The manager is in charge of managing relationship between
// components. Basically, it receives new commits from the fetcher,
// call the builder to evaluate and build them. Finally, it submits
// these builds to the deployer.

package manager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/executor"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/lease"
	"github.com/nlewo/comin/internal/leasestate"
	"github.com/nlewo/comin/internal/profile"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// DefaultPollPeriod is how often the manager re-evaluates release (C3) and
// drift (S4) state even when nothing was fetched, built, or deployed. It is
// exported so tests can shrink it with SetPollPeriod.
const DefaultPollPeriod = 30 * time.Second

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

	isSuspended bool

	broker *broker.Broker

	// leaseReader observes the override lease file (S1). A disabled
	// reader (empty path) turns off the lease behaviours: the deploy
	// gate admits everything, and nothing is deferred, released or
	// returned. Drift status and switch-latest stay on.
	leaseReader *lease.Reader
	// leaseState is comin's own cross-restart bookkeeping for the
	// override-lease feature (lease-aware/released marks, drift-since,
	// a pending switch-latest request); never part of store.json.
	leaseState *leasestate.State
	// pollPeriod bounds how long C3/S4 can go unevaluated when nothing
	// else wakes the manager's loop.
	pollPeriod time.Duration

	// deferred is the latest main generation C1 refused while a lease
	// was held; offerDeferred offers it once the lease is gone.
	deferredMu sync.Mutex
	deferred   *protobuf.Generation
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
	leaseReader *lease.Reader,
	leaseState *leasestate.State,
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
		leaseReader:             leaseReader,
		leaseState:              leaseState,
		pollPeriod:              DefaultPollPeriod,
	}
	deployer.SetAdmission(m.admitQueued)
	// A switch-latest request made before a restart and never resolved
	// is re-armed here, before the gRPC server can accept a new one, so
	// a request arriving during startup is not deployed twice.
	if leaseState.PendingSwitchLatest() {
		deployer.SubmitLatest(m.resolveSwitchLatest)
	}
	return m
}

// SetPollPeriod overrides the default poll period. It must be called before
// Run.
func (m *Manager) SetPollPeriod(d time.Duration) {
	m.pollPeriod = d
}

func (m *Manager) GetState() *protobuf.State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m *Manager) toState() *protobuf.State {
	return &protobuf.State{
		NeedToReboot:    wrapperspb.Bool(m.needToReboot),
		IsSuspended:     wrapperspb.Bool(m.isSuspended),
		Builder:         m.Builder.State(),
		Deployer:        m.deployer.State(),
		Fetcher:         m.Fetcher.GetState(),
		Store:           m.storage.GetState(),
		BuildConfirmer:  m.BuildConfirmer.status(),
		DeployConfirmer: m.DeployConfirmer.status(),
		Drift:           m.driftStatus(time.Now().UTC()),
	}
}

// SwitchDeploymentLatest always deploys the S4 expected generation with
// "switch", resolved at the moment the deployer actually starts deploying
// it (not now): if a deployment is currently in flight, the expected
// generation may change by the time this request reaches the front of the
// queue (e.g. a testing deployment that just finished becomes expected
// under a live git lease). It fails with "manager: no previous deployment"
// when there is nothing to switch to. The request is persisted until the
// deployer resolves it, so a comin restart in between does not drop it.
func (m *Manager) SwitchDeploymentLatest() error {
	if _, err := m.resolveExpectedGeneration(); err != nil {
		return err
	}
	if err := m.leaseState.SetPendingSwitchLatest(true); err != nil {
		return err
	}
	m.deployer.SubmitLatest(m.resolveSwitchLatest)
	return nil
}

// resolveExpectedGeneration resolves the S4 expected generation.
func (m *Manager) resolveExpectedGeneration() (*protobuf.Generation, error) {
	dpl := m.expectedDeployment(m.storage.DeploymentList(), m.leaseReader.Observe().IsGit())
	if dpl == nil {
		return nil, fmt.Errorf("manager: no previous deployment")
	}
	return dpl.Generation, nil
}

// resolveSwitchLatest is the resolver of every switch-latest request
// (operator requests and C3 returns); the deployer calls it at deploy
// time. Resolving settles the request whether or not it finds a
// generation, so it clears the persisted pending flag either way.
func (m *Manager) resolveSwitchLatest() (*protobuf.Generation, error) {
	g, err := m.resolveExpectedGeneration()
	if cerr := m.leaseState.SetPendingSwitchLatest(false); cerr != nil {
		logrus.Errorf("manager: could not clear the pending switch-latest request: %s", cerr)
	}
	return g, err
}

func (m *Manager) Suspend() error {
	if m.isSuspended {
		return fmt.Errorf("the manager is already suspended")
	}
	if err := m.Builder.Suspend(); err != nil {
		return err
	}
	m.deployer.Suspend()
	m.isSuspended = true
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Suspend_{Suspend: &protobuf.Event_Suspend{}}})
	return nil
}

func (m *Manager) Resume(ctx context.Context) error {
	if !m.isSuspended {
		return fmt.Errorf("the manager is not suspended")
	}
	if err := m.Builder.Resume(ctx); err != nil {
		return err
	}
	m.deployer.Resume()
	m.isSuspended = false
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Resume_{Resume: &protobuf.Event_Resume{}}})
	return nil
}

// FetchAndBuild fetches new commits. If a new commit is available, it
// evaluates and builds the derivation. Once built, it pushes the
// generation on a channel which is consumed by the deployer.
func (m *Manager) FetchAndBuild(ctx context.Context) {
	go func() {
		for {
			select {
			case rs := <-m.Fetcher.RepositoryStatusCh:
				if !rs.SelectedCommitShouldBeSigned.GetValue() || rs.SelectedCommitSigned.GetValue() {
					logrus.Infof("manager: a generation is evaluating for commit %s", rs.SelectedCommitId)
					err := m.Builder.Eval(ctx, rs)
					if err != nil {
						logrus.Error(err)
					}
				} else {
					logrus.Infof("manager: the commit %s is not evaluated because it is not signed", rs.SelectedCommitId)
				}
			case generationUUID := <-m.Builder.EvaluationDone:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				if generation.EvalErr != "" {
					continue
				}
				if generation.MachineId != "" && m.machineId != generation.MachineId {
					logrus.Infof("manager: the comin.machineId %s is not the host machine-id %s", generation.MachineId, m.machineId)
				} else {
					logrus.Infof("manager: the build of the generation %s is submitted", generation.Uuid)
					m.BuildConfirmer.Submit(generationUUID)
				}
			case generationUUID := <-m.BuildConfirmer.confirmed:
				m.Builder.SubmitBuild(ctx, generationUUID)

			case generationUUID := <-m.Builder.BuildDone:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				if generation.BuildErr == "" {
					logrus.Infof("manager: a generation is available for deployment with commit %s", generation.SelectedCommitId)
					if !m.deployer.IsAlreadyDeployed(&generation) {
						m.DeployConfirmer.Submit(generationUUID)
					}
				}
			case generationUUID := <-m.DeployConfirmer.confirmed:
				generation, err := m.storage.GenerationGet(generationUUID)
				if err != nil {
					logrus.Error(err)
					continue
				}
				operation, ok := m.leaseDeployDecision(&generation)
				if !ok {
					continue
				}
				m.deployer.Submit(&generation, operation)
			}
		}
	}()
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
	m.prometheus.SetHostInfo(m.needToReboot, m.isSuspended)

	m.FetchAndBuild(ctx)
	m.deployer.Run(ctx)

	ticker := time.NewTicker(m.pollPeriod)
	defer ticker.Stop()
	// C3, the deferred tier generation and S4 must be evaluated at least
	// once per poll period even when the fetcher emits nothing (it only
	// emits when the selected commit changes), so a lease that ends with
	// no new commits still returns within one poll period.
	m.poll(time.Now().UTC())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(time.Now().UTC())
		case <-m.stateRequestCh:
			m.stateResultCh <- m.toState()
		case dpl := <-m.deployer.DeploymentDoneCh:
			if m.leaseReader.Enabled() && isTestingDeployment(dpl) {
				if err := m.leaseState.MarkLeaseAware(dpl.Uuid); err != nil {
					logrus.Errorf("manager: could not mark deployment %s lease-aware: %s", dpl.Uuid, err)
				}
			}
			m.prometheus.SetDeploymentInfo(dpl.Generation.SelectedCommitId, dpl.Status)
			getsEvicted, evicted := m.storage.DeploymentInsertAndCommit(dpl)

			if getsEvicted {
				// We remove the evicted deployment profile
				// path only if this profile path is not used
				// by any still alive other deployments, and
				// its lease marks only once no row of it is
				// left in the store.
				uuidAlive, profileAlive := false, false
				for _, d := range m.storage.DeploymentList() {
					if d.Uuid == evicted.Uuid {
						uuidAlive = true
					}
					if d.ProfilePath == evicted.ProfilePath {
						profileAlive = true
					}
				}
				if evicted.ProfilePath != "" && !profileAlive {
					_ = profile.RemoveProfilePath(evicted.ProfilePath)
				}
				if !uuidAlive {
					if err := m.leaseState.Forget(evicted.Uuid); err != nil {
						logrus.Errorf("manager: could not forget the marks of the evicted deployment %s: %s", evicted.Uuid, err)
					}
				}
			}
			m.needToReboot = m.executor.NeedToReboot(dpl.Generation.OutPath, dpl.Operation)
			if m.needToReboot {
				e := &protobuf.Event_RebootRequired{Deployment: dpl}
				m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_RebootRequired_{RebootRequired: e}})
			}
			m.prometheus.SetHostInfo(m.needToReboot, m.isSuspended)
			// A deployment has just finished: a lease could now be
			// releasable (C3), a deferred generation offerable, and
			// the drift episode has changed, even before the next tick.
			m.poll(time.Now().UTC())
			if dpl.RestartComin.GetValue() {
				// TODO: stop contexts
				logrus.Infof("manager: comin needs to be restarted")
				logrus.Infof("manager: exiting comin to let the service manager restart it")
				os.Exit(0)
			}
		}
	}
}
