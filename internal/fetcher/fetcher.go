package fetcher

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/syncgate"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Fetcher struct {
	isFetching       atomic.Bool
	repositoryStatus *protobuf.RepositoryStatus
	mu               sync.RWMutex
	submitRemotes    chan request
	fetched          chan struct{}
	repo             repository.Repository
	gate             syncgate.Command
}

// request is a fetch of remotes asked for by a trigger.
type request struct {
	remotes []string
	trigger syncgate.Trigger
}

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithSyncGate asks c before every fetch start; a closed gate skips the fetch.
func WithSyncGate(c syncgate.Command) Option {
	return func(f *Fetcher) { f.gate = c }
}

func NewFetcher(repo repository.Repository, opts ...Option) *Fetcher {
	f := &Fetcher{
		repo:          repo,
		submitRemotes: make(chan request),
		fetched:       make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(f)
	}
	f.repositoryStatus = repo.GetRepositoryStatus()
	return f
}

func (f *Fetcher) IsFetching() bool {
	return f.isFetching.Load()
}

// TriggerFetch requests a fetch of remotes, asked from the sync gate with
// the poll trigger.
func (f *Fetcher) TriggerFetch(remotes []string) {
	f.submitRemotes <- request{remotes: remotes, trigger: syncgate.Poll}
}

// TriggerFetchManual requests a fetch of remotes, asked from the sync gate
// with the manual trigger.
func (f *Fetcher) TriggerFetchManual(remotes []string) {
	f.submitRemotes <- request{remotes: remotes, trigger: syncgate.Manual}
}

// Latest returns a copy of the repository status of the last completed fetch.
func (f *Fetcher) Latest() *protobuf.RepositoryStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return proto.CloneOf(f.repositoryStatus)
}

// Fetched returns a channel signalled after every completed fetch, failed or not.
// Its capacity is one, so signals pending together coalesce; it has a single
// reader, as a second receiver would take signals from the first.
func (f *Fetcher) Fetched() <-chan struct{} {
	return f.fetched
}

type RemoteState struct {
	Name      string    `json:"name"`
	FetchedAt time.Time `json:"fetched_at"`
}

type State struct {
	IsFetching       bool
	RepositoryStatus *protobuf.RepositoryStatus
}

func (f *Fetcher) GetState() *protobuf.Fetcher {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return &protobuf.Fetcher{
		IsFetching:       wrapperspb.Bool(f.isFetching.Load()),
		RepositoryStatus: proto.CloneOf(f.repositoryStatus),
	}
}

func (f *Fetcher) Start(ctx context.Context) {
	logrus.Info("fetcher: starting")
	go func() {
		remotes := make([]string, 0)
		trigger := syncgate.Poll
		var fetchResultCh chan *protobuf.RepositoryStatus
		for {
			select {
			case submitted := <-f.submitRemotes:
				logrus.Debugf("fetch: remotes submitted: %s", submitted.remotes)
				remotes = union(remotes, submitted.remotes)
				if submitted.trigger == syncgate.Manual {
					trigger = syncgate.Manual
				}
			case rs := <-fetchResultCh:
				f.isFetching.Store(false)
				f.mu.Lock()
				f.repositoryStatus = rs
				f.mu.Unlock()
				select {
				case f.fetched <- struct{}{}:
				default:
				}
			}
			if !f.isFetching.Load() && len(remotes) != 0 {
				if a := f.gate.Ask(ctx, syncgate.StageFetch, trigger); !a.Open {
					logrus.Infof("fetcher: the fetch of %s is skipped: sync gate: %s", remotes, a.Reason)
					remotes, trigger = []string{}, syncgate.Poll
					continue
				}
				f.isFetching.Store(true)
				fetchResultCh = f.repo.FetchAndUpdate(ctx, remotes)
				remotes, trigger = []string{}, syncgate.Poll
			}
		}
	}()
}

func union(array1, array2 []string) []string {
	for _, e2 := range array2 {
		exist := slices.Contains(array1, e2)
		if !exist {
			array1 = append(array1, e2)
		}
	}
	return array1
}
