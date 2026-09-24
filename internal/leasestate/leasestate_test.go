package leasestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMarksPersistAcrossRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s.IsLeaseAware("d1"))
	assert.NoError(t, s.MarkLeaseAware("d1"))
	assert.NoError(t, s.MarkReleased("d1"))

	// Simulate a restart: load a fresh State from the same file.
	s2, err := Load(p)
	assert.NoError(t, err)
	assert.True(t, s2.IsLeaseAware("d1"))
	assert.True(t, s2.IsReleased("d1"))
	assert.False(t, s2.IsLeaseAware("d2"))
}

func TestDriftSincePersistsAndClears(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.Nil(t, s.DriftSince())

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.NoError(t, s.SetDriftSince(&since))

	s2, err := Load(p)
	assert.NoError(t, err)
	assert.NotNil(t, s2.DriftSince())
	assert.True(t, since.Equal(*s2.DriftSince()))

	assert.NoError(t, s2.SetDriftSince(nil))
	s3, err := Load(p)
	assert.NoError(t, err)
	assert.Nil(t, s3.DriftSince())
}

func TestPendingSwitchLatestPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s.PendingSwitchLatest())
	assert.NoError(t, s.SetPendingSwitchLatest(true))

	s2, err := Load(p)
	assert.NoError(t, err)
	assert.True(t, s2.PendingSwitchLatest())

	assert.NoError(t, s2.SetPendingSwitchLatest(false))
	s3, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s3.PendingSwitchLatest())
}

func TestLoadMissingFileStartsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s.IsLeaseAware("x"))
	assert.False(t, s.IsReleased("x"))
	assert.Nil(t, s.DriftSince())
	assert.False(t, s.PendingSwitchLatest())
}

// A file cut short (a crash mid-write by an older comin, a full disk) is
// reported, and yields no marks: nothing is released. Mutant: treat it as
// a missing file (silent reset).
func TestLoadTruncatedFileIsReported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.NoError(t, s.MarkLeaseAware("d1"))
	assert.NoError(t, s.MarkReleased("d1"))
	content, err := os.ReadFile(p)
	assert.NoError(t, err)

	for _, cut := range []int{0, len(content) / 2, len(content) - 1} {
		assert.NoError(t, os.WriteFile(p, content[:cut], 0644))
		s2, err := Load(p)
		assert.Error(t, err, "cut at %d bytes", cut)
		assert.NotNil(t, s2)
		assert.False(t, s2.IsLeaseAware("d1"))
		assert.False(t, s2.IsReleased("d1"))
	}
}

// Every write replaces the file atomically: a reader (or a crash) at any
// instant sees a complete state, never a truncated one. Mutant: write the
// file in place.
func TestCommitNeverExposesAPartialFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	for i := range 100 {
		assert.NoError(t, s.MarkLeaseAware(fmt.Sprintf("deployment-%04d-%s", i, strings.Repeat("x", 800))))
	}

	stop := make(chan struct{})
	partial := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(partial)
				return
			default:
			}
			content, err := os.ReadFile(p)
			if err == nil && !json.Valid(content) {
				partial <- fmt.Sprintf("%d bytes", len(content))
				close(partial)
				<-stop
				return
			}
		}
	}()
	for i := range 200 {
		assert.NoError(t, s.MarkReleased(fmt.Sprintf("r%d", i)))
	}
	close(stop)
	if got, ok := <-partial; ok {
		t.Fatalf("a reader saw a partial lease-state file (%s)", got)
	}
}

func TestForgetDropsMarks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.NoError(t, s.MarkLeaseAware("d1"))
	assert.NoError(t, s.MarkReleased("d1"))
	assert.NoError(t, s.MarkLeaseAware("d2"))
	assert.NoError(t, s.Forget("d1"))

	s2, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s2.IsLeaseAware("d1"))
	assert.False(t, s2.IsReleased("d1"))
	assert.True(t, s2.IsLeaseAware("d2"))
}
