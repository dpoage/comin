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
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Fetcher struct {
	isFetching         atomic.Bool
	repositoryStatus   *protobuf.RepositoryStatus
	mu                 sync.RWMutex
	submitRemotes      chan []string
	RepositoryStatusCh chan *protobuf.RepositoryStatus
	repo               repository.Repository
	// testingSelection, when set, is asked for the testing selection
	// of every fetch.
	testingSelection func() repository.TestingSelection
	// mainHead is the main commit of the latest fetch result.
	mainHead string
}

func NewFetcher(repo repository.Repository) *Fetcher {
	f := &Fetcher{
		repo:               repo,
		submitRemotes:      make(chan []string),
		RepositoryStatusCh: make(chan *protobuf.RepositoryStatus),
	}
	f.repositoryStatus = repo.GetRepositoryStatus()
	return f

}

func (f *Fetcher) IsFetching() bool {
	return f.isFetching.Load()
}

// SetTestingSelection installs the func asked for the testing selection
// of every fetch. Without one, fetches use the default selection.
func (f *Fetcher) SetTestingSelection(selection func() repository.TestingSelection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testingSelection = selection
}

// MainHead returns the main commit of the latest fetch result, whether or
// not that fetch changed the selected commit. It is empty before the
// first fetch completes.
func (f *Fetcher) MainHead() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.mainHead
}

// IsAncestor reports whether the commit base is the commit top or one of
// its ancestors in the local repository.
func (f *Fetcher) IsAncestor(base, top string) (bool, error) {
	return f.repo.IsAncestor(base, top)
}

func (f *Fetcher) TriggerFetch(remotes []string) {
	f.submitRemotes <- remotes
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
		RepositoryStatus: f.repo.GetRepositoryStatus(),
	}
}

func (f *Fetcher) Start(ctx context.Context) {
	logrus.Info("fetcher: starting")
	go func() {
		remotes := make([]string, 0)
		var workerRepositoryStatusCh chan *protobuf.RepositoryStatus
		for {
			select {
			case submittedRemotes := <-f.submitRemotes:
				logrus.Debugf("fetch: remotes submitted: %s", submittedRemotes)
				remotes = union(remotes, submittedRemotes)
			case rs := <-workerRepositoryStatusCh:
				f.isFetching.Store(false)
				f.mu.Lock()
				f.mainHead = rs.MainCommitId
				if rs.SelectedCommitId != f.repositoryStatus.SelectedCommitId || rs.SelectedBranchIsTesting.GetValue() != f.repositoryStatus.SelectedBranchIsTesting.GetValue() {
					f.repositoryStatus = rs
					f.RepositoryStatusCh <- rs
				}
				f.mu.Unlock()
			}
			if !f.isFetching.Load() && len(remotes) != 0 {
				f.isFetching.Store(true)
				f.mu.RLock()
				selection := f.testingSelection
				f.mu.RUnlock()
				var testing repository.TestingSelection
				if selection != nil {
					testing = selection()
				}
				workerRepositoryStatusCh = f.repo.FetchAndUpdate(ctx, remotes, testing)
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
