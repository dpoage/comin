package builder

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type runnableGate struct {
	release chan struct{}
}

func (r *runnableGate) Run(ctx context.Context) error {
	<-r.release
	return nil
}

// Every pending Wait, and a Wait after the end, returns when the runnable ends.
func TestExecWaitersAllReturnWhenTheRunnableEnds(t *testing.T) {
	r := &runnableGate{release: make(chan struct{})}
	e := NewExec(r, time.Minute)
	e.Start(t.Context())

	const waiters = 3
	var wg sync.WaitGroup
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Wait()
		}()
	}
	// Let the waiters block on the channel before the runnable ends.
	time.Sleep(200 * time.Millisecond)
	close(r.release)

	returned := make(chan struct{})
	go func() {
		wg.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a Wait was not released when the runnable ended")
	}

	// A Wait after the end returns at once.
	e.Wait()
	assert.True(t, e.finished.Load())
}
