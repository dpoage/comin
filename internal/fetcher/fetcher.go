package fetcher

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Fetcher struct {
	isFetching       atomic.Bool
	repositoryStatus *protobuf.RepositoryStatus
	mu               sync.RWMutex
	submitRemotes    chan []string
	fetched          chan struct{}
	repo             repository.Repository
}

func NewFetcher(repo repository.Repository) *Fetcher {
	f := &Fetcher{
		repo:          repo,
		submitRemotes: make(chan []string),
		fetched:       make(chan struct{}, 1),
	}
	f.repositoryStatus = repo.GetRepositoryStatus()
	return f
}

func (f *Fetcher) IsFetching() bool {
	return f.isFetching.Load()
}

func (f *Fetcher) TriggerFetch(remotes []string) {
	f.submitRemotes <- remotes
}

// Latest returns a copy of the repository status of the last completed
// fetch.
func (f *Fetcher) Latest() *protobuf.RepositoryStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return proto.CloneOf(f.repositoryStatus)
}

// Fetched returns a channel signalled after every completed fetch,
// whether the selection changed, did not change, or the fetch failed.
// The channel has a capacity of one: signals of fetches completed while
// a signal is still pending are coalesced into it. It has a single reader:
// a second receiver would take signals from the first.
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
		var fetchResultCh chan *protobuf.RepositoryStatus
		for {
			select {
			case submittedRemotes := <-f.submitRemotes:
				logrus.Debugf("fetch: remotes submitted: %s", submittedRemotes)
				remotes = union(remotes, submittedRemotes)
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
				f.isFetching.Store(true)
				fetchResultCh = f.repo.FetchAndUpdate(ctx, remotes)
				remotes = []string{}
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
