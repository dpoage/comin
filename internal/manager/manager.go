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
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
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

	isSuspended atomic.Bool

	broker *broker.Broker

	// preflight is the externally configured admission check.
	preflight preflight.Command

	// tick is the period of the passes that need no event.
	tick time.Duration
	exit func(code int)
	// wakeCh requests a pass; triggerCh requests one with the given trigger.
	wakeCh    chan struct{}
	triggerCh chan string

	// cache and cacheSeq belong to the pass goroutine.
	cache    map[genKey]*cached
	cacheSeq int

	// mu guards the fields below.
	mu              sync.Mutex
	buildPending    string
	deployPending   string
	deployConfirmed string
	inFlight        *submission
	lastActivation  *reconcile.Activation
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
		exit:                    os.Exit,
		wakeCh:                  make(chan struct{}, 1),
		triggerCh:               make(chan string, 1),
		cache:                   map[genKey]*cached{},
	}
	return m
}

func (m *Manager) GetState() *protobuf.State {
	m.stateRequestCh <- struct{}{}
	return <-m.stateResultCh
}

func (m *Manager) toState() *protobuf.State {
	return &protobuf.State{
		NeedToReboot:    wrapperspb.Bool(m.needToReboot),
		IsSuspended:     wrapperspb.Bool(m.isSuspended.Load()),
		Builder:         m.Builder.State(),
		Deployer:        m.deployer.State(),
		Fetcher:         m.Fetcher.GetState(),
		Store:           m.storage.GetState(),
		BuildConfirmer:  m.BuildConfirmer.status(),
		DeployConfirmer: m.DeployConfirmer.status(),
	}
}

func (m *Manager) SwitchDeploymentLatest() error {
	ok, latest := m.storage.LastDeployment()
	if !ok {
		return fmt.Errorf("manager: no previous deployment")
	}
	m.deployer.Submit(latest.Generation, "switch")
	return nil
}

func (m *Manager) Suspend() error {
	if m.isSuspended.Load() {
		return fmt.Errorf("the manager is already suspended")
	}
	if err := m.Builder.Suspend(); err != nil {
		return err
	}
	m.deployer.Suspend()
	m.isSuspended.Store(true)
	m.broker.Publish(&protobuf.Event{Type: &protobuf.Event_Suspend_{Suspend: &protobuf.Event_Suspend{}}})
	return nil
}

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

	m.deployer.SetPreActivation(m.preActivation)
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
