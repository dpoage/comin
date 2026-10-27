package fetcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
)

func TestFetcher(t *testing.T) {
	r := utils.NewRepositoryMock()
	f := NewFetcher(r)
	f.Start(t.Context())
	var commitId string

	for i := range 2 {
		assert.False(t, f.IsFetching())
		f.TriggerFetch([]string{"remote"})

		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			assert.True(c, f.IsFetching())
		}, 5*time.Second, 100*time.Millisecond, "fetcher is not fetching")

		// This is to simulate a git fetch
		commitId = fmt.Sprintf("id-%d", i)
		r.RsCh <- &protobuf.RepositoryStatus{
			SelectedCommitId: commitId,
		}
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			<-f.Fetched()
			assert.Equal(c, commitId, f.Latest().SelectedCommitId)
		}, 5*time.Second, 100*time.Millisecond, "fetcher failed to fetch")

		assert.False(t, f.IsFetching())
	}

	f.TriggerFetch([]string{"remote"})
	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId: "id-5",
	}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		<-f.Fetched()
		assert.Equal(c, "id-5", f.Latest().SelectedCommitId)
	}, 5*time.Second, 100*time.Millisecond, "fetcher failed to fetch")

	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId: "id-5",
	}
	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId: "id-6",
	}
	assert.Eventually(t, func() bool { return f.Latest().SelectedCommitId != "id-5" }, 5*time.Second, 50*time.Millisecond)
}

// TestFetcherFetchedSignal asserts a signal is delivered after every
// completed fetch — whether the selection changed, did not change, or
// the fetch failed — and that Latest then returns that fetch's status.
func TestFetcherFetchedSignal(t *testing.T) {
	r := utils.NewRepositoryMock()
	f := NewFetcher(r)
	f.Start(t.Context())

	// No signal before any fetch completed.
	select {
	case <-f.Fetched():
		t.Fatal("signal before any completed fetch")
	default:
	}

	// A fetch that changes the selection
	f.TriggerFetch([]string{"remote"})
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, f.IsFetching())
	}, 5*time.Second, 100*time.Millisecond, "fetcher is not fetching")
	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId:  "id-1",
		SelectedCommitMsg: "changed",
	}
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, "id-1", f.Latest().SelectedCommitId)
	}, 5*time.Second, 100*time.Millisecond, "fetcher failed to fetch")
	awaitFetched(t, f)
	assert.Equal(t, "changed", f.Latest().SelectedCommitMsg)

	// A fetch that changes nothing: a signal, Latest is
	// the new status.
	f.TriggerFetch([]string{"remote"})
	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId:  "id-1",
		SelectedCommitMsg: "unchanged",
	}
	awaitFetched(t, f)
	select {
	case <-f.Fetched():
		t.Fatal("a second Fetched signal for one completed fetch")
	case <-time.After(500 * time.Millisecond):
	}

	// A fetch that fails: a signal, Latest is the failed fetch's status.
	f.TriggerFetch([]string{"remote"})
	r.RsCh <- &protobuf.RepositoryStatus{
		SelectedCommitId:  "id-1",
		SelectedCommitMsg: "unchanged",
		Remotes: []*protobuf.Remote{{
			Name:          "remote",
			FetchErrorMsg: "'git fetch remote' fails",
		}},
	}
	awaitFetched(t, f)
	assert.Equal(t, "'git fetch remote' fails", f.Latest().Remotes[0].FetchErrorMsg)
	select {
	case <-f.Fetched():
		t.Fatal("a second Fetched signal for one completed fetch")
	case <-time.After(500 * time.Millisecond):
	}
	assert.False(t, f.IsFetching())
}

// awaitFetched fails the test unless the fetcher signals a completed
// fetch within five seconds.
func awaitFetched(t *testing.T, f *Fetcher) {
	t.Helper()
	select {
	case <-f.Fetched():
	case <-time.After(5 * time.Second):
		t.Fatal("no Fetched signal after a completed fetch")
	}
}

func TestUnion(t *testing.T) {
	res := union([]string{"r1", "r2"}, []string{"r1", "r3"})
	assert.Equal(t, []string{"r1", "r2", "r3"}, res)
}
