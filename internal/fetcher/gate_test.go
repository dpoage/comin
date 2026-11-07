package fetcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/syncgate"
	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gateScript struct {
	dir string
	cmd syncgate.Command
}

// newGate writes a gate that logs "<stage> <trigger>" and is closed while
// the file "closed" exists.
func newGate(t *testing.T) gateScript {
	t.Helper()
	dir := t.TempDir()
	body := `#!/bin/sh
echo "$COMIN_SYNC_STAGE $COMIN_TRIGGER" >> ` + filepath.Join(dir, "log") + `
if [ -e ` + filepath.Join(dir, "closed") + ` ]; then echo "window closed"; exit 1; fi
`
	p := filepath.Join(dir, "gate.sh")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	return gateScript{dir: dir, cmd: syncgate.Command{Path: p, Timeout: 5 * time.Second}}
}

func (g gateScript) close(t *testing.T, closed bool) {
	t.Helper()
	p := filepath.Join(g.dir, "closed")
	if closed {
		require.NoError(t, os.WriteFile(p, nil, 0o644))
		return
	}
	_ = os.Remove(p)
}

func (g gateScript) log() string {
	b, _ := os.ReadFile(filepath.Join(g.dir, "log"))
	return strings.TrimSpace(string(b))
}

func TestFetchGate(t *testing.T) {
	t.Run("a closed gate skips the fetch without a signal or an error", func(t *testing.T) {
		g := newGate(t)
		g.close(t, true)
		r := utils.NewRepositoryMock()
		f := NewFetcher(r, WithSyncGate(g.cmd))
		f.Start(t.Context())

		f.TriggerFetch([]string{"remote"})
		f.TriggerFetchManual([]string{"remote"})
		assert.Never(t, f.IsFetching, 500*time.Millisecond, 25*time.Millisecond, "a fetch started under a closed gate")
		assert.Equal(t, "fetch poll\nfetch manual", g.log())
		select {
		case <-f.Fetched():
			t.Fatal("a skipped fetch signals Fetched")
		default:
		}
		assert.Empty(t, f.Latest().Remotes)
		assert.Empty(t, f.GetState().RepositoryStatus.GetSelectedCommitId())
	})

	t.Run("the next fetch after the gate opens runs", func(t *testing.T) {
		g := newGate(t)
		g.close(t, true)
		r := utils.NewRepositoryMock()
		f := NewFetcher(r, WithSyncGate(g.cmd))
		f.Start(t.Context())
		f.TriggerFetch([]string{"remote"})
		assert.Never(t, f.IsFetching, 300*time.Millisecond, 25*time.Millisecond)

		g.close(t, false)
		f.TriggerFetch([]string{"remote"})
		assert.Eventually(t, f.IsFetching, 5*time.Second, 25*time.Millisecond, "the fetch did not start once the gate opened")
		r.RsCh <- &protobuf.RepositoryStatus{SelectedCommitId: "id-1"}
		awaitFetched(t, f)
		assert.Equal(t, "id-1", f.Latest().SelectedCommitId)
	})

	t.Run("the trigger of the fetch is asked", func(t *testing.T) {
		g := newGate(t)
		r := utils.NewRepositoryMock()
		f := NewFetcher(r, WithSyncGate(g.cmd))
		f.Start(t.Context())

		f.TriggerFetch([]string{"remote"})
		assert.Eventually(t, f.IsFetching, 5*time.Second, 25*time.Millisecond)
		r.RsCh <- &protobuf.RepositoryStatus{SelectedCommitId: "id-1"}
		awaitFetched(t, f)

		f.TriggerFetchManual([]string{"remote"})
		assert.Eventually(t, f.IsFetching, 5*time.Second, 25*time.Millisecond)
		r.RsCh <- &protobuf.RepositoryStatus{SelectedCommitId: "id-2"}
		awaitFetched(t, f)

		// A poll fetch that follows leaves the manual trigger behind.
		f.TriggerFetch([]string{"remote"})
		assert.Eventually(t, f.IsFetching, 5*time.Second, 25*time.Millisecond)
		r.RsCh <- &protobuf.RepositoryStatus{SelectedCommitId: "id-3"}
		awaitFetched(t, f)
		assert.Equal(t, "fetch poll\nfetch manual\nfetch poll", g.log())
	})

	t.Run("no gate configured asks nothing", func(t *testing.T) {
		g := newGate(t)
		r := utils.NewRepositoryMock()
		f := NewFetcher(r)
		f.Start(t.Context())
		f.TriggerFetchManual([]string{"remote"})
		assert.Eventually(t, f.IsFetching, 5*time.Second, 25*time.Millisecond)
		assert.Empty(t, g.log())
	})
}
