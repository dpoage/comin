package deployer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dustin/go-humanize"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	ReasonDeploymentBuilder = "builder"
	ReasonDeploymentManual  = "manual"
	// ReasonDeploymentSwitchLatest tags a deployment produced by `comin
	// deployment switch-latest`, so callers can tell it apart from an
	// ordinary fetched-and-built deployment.
	ReasonDeploymentSwitchLatest = "switch-latest"
)

type DeployFunc func(context.Context, string, string) (bool, string, error)

// ResolveGenerationFunc resolves the generation to deploy at the moment the
// deployer is ready to start deploying it, not when the request was made.
type ResolveGenerationFunc func() (*protobuf.Generation, error)

// AdmissionFunc reports whether a generation queued by Submit may start
// deploying now. It is consulted right before the deployer starts it, so a
// decision taken when the generation was submitted can be revisited.
type AdmissionFunc func(*protobuf.Generation) bool

type Deployer struct {
	GenerationCh       chan *protobuf.Generation
	deployerFunc       DeployFunc
	DeploymentDoneCh   chan *protobuf.Deployment
	mu                 sync.Mutex
	deployment         atomic.Pointer[protobuf.Deployment]
	previousDeployment atomic.Pointer[protobuf.Deployment]
	isDeploying        atomic.Bool
	// The next generation to deploy. nil when there is no new generation to deploy
	GenerationToDeploy *protobuf.Generation
	// The operation to use for the next deployment
	Operation string
	// Reason is the reason of the next deployment
	Reason string
	// resolveLatest is a queued switch-latest request. It is independent
	// of GenerationToDeploy (neither displaces the other) and runs first
	// when both are queued. It is called once the deployer is about to
	// start deploying, so the request targets whatever is "expected" at
	// that moment, not at submission time.
	resolveLatest ResolveGenerationFunc
	// starting is true between the moment the deployer takes a request
	// off the queue and the moment its deployment is in flight (or the
	// request is abandoned), so Idle never reports a gap between the two.
	starting              bool
	admit                 AdmissionFunc
	generationAvailableCh chan struct{}
	postDeploymentCommand string

	isSuspended atomic.Bool
	resumeCh    chan struct{}
	// This is true when the runner is actually suspended. This is
	// mainly used for testing purpose.
	runnerIsSuspended atomic.Bool
	store             *store.Store
}

func (d *Deployer) State() *protobuf.Deployer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &protobuf.Deployer{
		IsDeploying:        wrapperspb.Bool(d.isDeploying.Load()),
		GenerationToDeploy: d.GenerationToDeploy,
		Operation:          d.Operation,
		Deployment:         d.deployment.Load(),
		PreviousDeployment: d.previousDeployment.Load(),
		IsSuspended:        wrapperspb.Bool(d.isSuspended.Load()),
	}
}

func (d *Deployer) Deployment() *protobuf.Deployment {
	return d.deployment.Load()
}

func (d *Deployer) IsDeploying() bool {
	return d.isDeploying.Load()
}

func (d *Deployer) RunnerIsSuspended() bool {
	return d.runnerIsSuspended.Load()
}

func (d *Deployer) IsSuspended() bool {
	return d.isSuspended.Load()
}

func showDeployment(padding string, d *protobuf.Deployment) {
	switch d.Status {
	case store.StatusToString(store.Running):
		fmt.Printf("%sDeployment is running since %s\n", padding, humanize.Time(d.StartedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
	case store.StatusToString(store.Done):
		fmt.Printf("%sDeployment succeeded %s\n", padding, humanize.Time(d.EndedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
		fmt.Printf("%sProfilePath %s\n", padding, d.ProfilePath)
	case store.StatusToString(store.Failed):
		fmt.Printf("%sDeployment failed %s\n", padding, humanize.Time(d.EndedAt.AsTime()))
		fmt.Printf("%sOperation %s\n", padding, d.Operation)
		fmt.Printf("%sProfilePath %s\n", padding, d.ProfilePath)
	}
	fmt.Printf("%sGeneration %s\n", padding, d.Generation.Uuid)
	fmt.Printf("%sCommit ID %s from %s/%s\n", padding, d.Generation.SelectedCommitId, d.Generation.SelectedRemoteName, d.Generation.SelectedBranchName)
	fmt.Printf("%sCommit message %s\n", padding, strings.Trim(d.Generation.SelectedCommitMsg, "\n"))
	fmt.Printf("%sOutpath %s\n", padding, d.Generation.OutPath)
}

func Show(s *protobuf.Deployer, padding string) {
	fmt.Printf("  Deployer\n")
	if s.Deployment == nil {
		if s.PreviousDeployment == nil {
			fmt.Printf("%sNo deployment yet\n", padding)
			return
		}
		showDeployment(padding, s.PreviousDeployment)
		return
	}
	showDeployment(padding, s.Deployment)
}

func New(store *store.Store, deployFunc DeployFunc, previousDeployment *protobuf.Deployment, postDeploymentCommand string) *Deployer {
	if previousDeployment != nil {
		logrus.Infof("deployer: initializing with previous deployment %s", previousDeployment.Uuid)
	}
	deployer := &Deployer{
		store:                 store,
		DeploymentDoneCh:      make(chan *protobuf.Deployment, 1),
		deployerFunc:          deployFunc,
		generationAvailableCh: make(chan struct{}, 1),
		postDeploymentCommand: postDeploymentCommand,

		resumeCh: make(chan struct{}, 1),
	}
	deployer.previousDeployment.Store(previousDeployment)
	deployer.deployment.Store(previousDeployment)

	return deployer
}

func (d *Deployer) Suspend() {
	d.isSuspended.Store(true)
}

func (d *Deployer) Resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.isSuspended.Store(false)
	select {
	case d.resumeCh <- struct{}{}:
	default:
	}
}

func (d *Deployer) IsAlreadyDeployed(generation *protobuf.Generation) bool {
	previous := d.previousDeployment.Load()
	if previous == nil || generation.SelectedCommitId != previous.Generation.SelectedCommitId || generation.SelectedBranchIsTesting.GetValue() != previous.Generation.SelectedBranchIsTesting.GetValue() {
		return false
	} else {
		logrus.Infof("deployer: skipping deployment of the generation %s because it is the same than the last deployment", generation.Uuid)
		return true
	}
}

// Submit submits a generation to be deployed. If a deployment is
// running, this generation will be deployed once the current
// deployment is finished. If this generation is the same than the one
// of the last deployment, this generation is skipped. A later Submit
// replaces a generation still waiting in the queue; a queued
// switch-latest request (SubmitLatest) is left untouched.
func (d *Deployer) Submit(generation *protobuf.Generation, operation string) {
	logrus.Infof("deployer: submitting generation %s with operation %s", generation.Uuid, operation)
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.IsAlreadyDeployed(generation) {
		d.GenerationToDeploy = generation
		d.Operation = operation
		d.signalLocked()
	}
}

// SubmitLatest queues a request whose target generation is resolved by
// resolve at the moment the deployer is ready to start deploying it, not
// when this call is made, and always deploys it with the "switch"
// operation. Unlike Submit, it is never skipped by the IsAlreadyDeployed
// check and never refused by the admission func: an explicit
// switch-latest request always produces a new deployment. A generation
// queued by Submit stays queued and is deployed after this request;
// several SubmitLatest calls made before the deployer starts the request
// produce one deployment.
func (d *Deployer) SubmitLatest(resolve ResolveGenerationFunc) {
	logrus.Infof("deployer: submitting a switch-latest request")
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolveLatest = resolve
	d.signalLocked()
}

// SetAdmission installs the func consulted before each generation queued
// by Submit starts deploying; a generation it refuses is dropped.
func (d *Deployer) SetAdmission(admit AdmissionFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.admit = admit
}

// Activity reports, as one snapshot, whether a deployment is in flight
// (its post-deployment command included) and whether a deployment request
// (concrete or a switch-latest resolver) is waiting for the deployer to
// start it. A request moves from queued to in flight under the same lock,
// so a caller never sees it in neither.
func (d *Deployer) Activity() (inFlight, queued bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isDeploying.Load(), d.hasQueuedWorkLocked()
}

func (d *Deployer) hasQueuedWorkLocked() bool {
	return d.GenerationToDeploy != nil || d.resolveLatest != nil || d.starting
}

// Idle reports whether nothing is queued and nothing is in flight. The
// deployment in flight includes its post-deployment command.
func (d *Deployer) Idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.isDeploying.Load() && !d.hasQueuedWorkLocked()
}

func (d *Deployer) signalLocked() {
	select {
	case d.generationAvailableCh <- struct{}{}:
	default:
	}
}

// takeLocked removes the next request from the queue: the switch-latest
// request first, else the concrete generation. It returns false when the
// queue is empty.
func (d *Deployer) takeLocked() (g *protobuf.Generation, operation, reason string, resolve ResolveGenerationFunc, ok bool) {
	if d.resolveLatest != nil {
		resolve = d.resolveLatest
		d.resolveLatest = nil
	} else if d.GenerationToDeploy != nil {
		g, operation, reason = d.GenerationToDeploy, d.Operation, d.Reason
		d.GenerationToDeploy = nil
	} else {
		return nil, "", "", nil, false
	}
	d.starting = true
	return g, operation, reason, resolve, true
}

// settle marks the current request finished or abandoned and wakes the
// runner again when more work is queued.
func (d *Deployer) settle(finished *protobuf.Deployment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starting = false
	d.isDeploying.Store(false)
	if finished != nil {
		d.deployment.Store(finished)
	}
	if d.hasQueuedWorkLocked() {
		d.signalLocked()
	}
}

func (d *Deployer) Run(ctx context.Context) {
	go func() {
		for {
			<-d.generationAvailableCh

			if d.isSuspended.Load() {
				d.runnerIsSuspended.Store(true)
				<-d.resumeCh
				d.runnerIsSuspended.Store(false)
			}

			d.mu.Lock()
			g, operation, reason, resolve, ok := d.takeLocked()
			admit := d.admit
			d.mu.Unlock()
			if !ok {
				continue
			}

			if resolve != nil {
				resolved, err := resolve()
				if err == nil && resolved == nil {
					err = fmt.Errorf("the resolver returned no generation")
				}
				if err != nil {
					logrus.Errorf("deployer: switch-latest could not resolve a generation to deploy: %s", err)
					d.settle(nil)
					continue
				}
				g = resolved
				operation = "switch"
				reason = ReasonDeploymentSwitchLatest
			} else if admit != nil && !admit(g) {
				logrus.Infof("deployer: the generation %s is no longer admitted for deployment", g.Uuid)
				d.settle(nil)
				continue
			}
			logrus.Infof("deployer: deploying generation %s with operation %s", g.Uuid, operation)

			dpl := d.store.NewDeployment(g, operation, reason)
			d.mu.Lock()
			d.previousDeployment.Swap(d.Deployment())
			d.deployment.Store(dpl)
			d.isDeploying.Store(true)
			d.starting = false
			d.mu.Unlock()
			started, err := d.store.DeploymentStarted(dpl.Uuid)
			if err != nil {
				logrus.Errorf("deployer: could not update the deployment %s in the store", dpl.Uuid)
				d.settle(nil)
				continue
			}
			d.deployment.Store(started)
			cominNeedRestart, profilePath, err := d.deployerFunc(
				ctx,
				g.OutPath,
				operation,
			)

			deployment, err := d.store.DeploymentFinished(dpl.Uuid, err, cominNeedRestart, profilePath)
			if err != nil {
				logrus.Errorf("deployer: could not update the deployment %s in the store", dpl.Uuid)
				d.settle(nil)
				continue
			}
			d.deployment.Store(deployment)
			cmd := d.postDeploymentCommand
			if cmd != "" {
				_, err = runPostDeploymentCommand(cmd, deployment)
				if err != nil {
					logrus.Errorf("deployer: deploying generation %s, post deployment command [%s] failed %v", g.Uuid, cmd, err)
				}
			}

			d.settle(deployment)
			d.DeploymentDoneCh <- deployment
		}
	}()
}
