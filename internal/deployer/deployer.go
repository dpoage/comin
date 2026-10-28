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
)

type DeployFunc func(context.Context, string, string) (bool, string, error)

// PreActivation is called on the runner goroutine after a submitted
// generation is dequeued and before its deployment is recorded. Returning
// false skips the deployment: nothing is recorded and the deployer waits for
// the next submission.
type PreActivation func(ctx context.Context, generation *protobuf.Generation, operation string) bool

type Deployer struct {
	preActivation      PreActivation
	abandoned          func(*protobuf.Generation)
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
	Reason                string
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

// Suspend takes the lock the runner holds while it decides to record a
// deployment: once Suspend returns, no deployment starts until Resume.
func (d *Deployer) Suspend() {
	d.mu.Lock()
	defer d.mu.Unlock()
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

// SetPreActivation installs check, which runs before every deployment, and
// abandoned, which the runner calls with a generation that check admitted
// while the deployer was suspended; that generation is not deployed.
// Both are required: a nil abandoned panics the runner. Call it before Run.
func (d *Deployer) SetPreActivation(check PreActivation, abandoned func(*protobuf.Generation)) {
	d.preActivation = check
	d.abandoned = abandoned
}

// Submit submits a generation to be deployed. If a deployment is
// running, this generation will be deployed once the current
// deployment is finished, replacing a generation still waiting.
func (d *Deployer) Submit(generation *protobuf.Generation, operation string) {
	logrus.Infof("deployer: submitting generation %s with operation %s", generation.Uuid, operation)
	d.mu.Lock()
	defer d.mu.Unlock()

	d.GenerationToDeploy = generation
	d.Operation = operation
	select {
	case d.generationAvailableCh <- struct{}{}:
	default:
	}
}

func (d *Deployer) Run(ctx context.Context) {
	go func() {
		for {
			<-d.generationAvailableCh

			d.mu.Lock()
			for d.isSuspended.Load() {
				d.mu.Unlock()
				d.runnerIsSuspended.Store(true)
				<-d.resumeCh
				d.runnerIsSuspended.Store(false)
				d.mu.Lock()
			}
			g := d.GenerationToDeploy
			operation := d.Operation
			d.GenerationToDeploy = nil
			if g == nil {
				d.mu.Unlock()
				continue
			}
			if d.preActivation != nil {
				d.mu.Unlock()
				if !d.preActivation(ctx, g, operation) {
					logrus.Infof("deployer: generation %s is not deployed: the pre-activation check refused it", g.Uuid)
					continue
				}
				d.mu.Lock()
				if d.isSuspended.Load() {
					d.mu.Unlock()
					logrus.Infof("deployer: generation %s is not deployed: the deployer was suspended during the pre-activation check", g.Uuid)
					d.abandoned(g)
					continue
				}
			}
			logrus.Infof("deployer: deploying generation %s with operation %s", g.Uuid, operation)

			dpl := d.store.NewDeployment(g, operation, d.Reason)
			d.previousDeployment.Swap(d.Deployment())
			d.deployment.Store(dpl)
			d.isDeploying.Store(true)
			d.mu.Unlock()
			started, err := d.store.DeploymentStarted(dpl.Uuid)
			if err != nil {
				logrus.Errorf("deployer: could not update the deployment %s in the store", dpl.Uuid)
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

			d.isDeploying.Store(false)
			d.DeploymentDoneCh <- d.Deployment()
		}
	}()
}
