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
	assert.NoError(t, s.Release([]string{"d1"}))

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
	assert.NoError(t, s.Release([]string{"d1"}))
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
		assert.NoError(t, s.Release([]string{fmt.Sprintf("r%d", i)}))
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
	assert.NoError(t, s.Release([]string{"d1"}))
	assert.NoError(t, s.MarkLeaseAware("d2"))
	assert.NoError(t, s.Forget("d1"))

	s2, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s2.IsLeaseAware("d1"))
	assert.False(t, s2.IsReleased("d1"))
	assert.True(t, s2.IsLeaseAware("d2"))
}

// Only a missing file is "absent at load": an existing file, readable or
// not, is not, and a missing file records no loss. Mutant: Load never sets
// absent.
func TestAbsentAtLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.True(t, s.AbsentAtLoad())
	assert.Nil(t, s.MarksLostAt())
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err), "Load writes nothing for a missing file")

	assert.NoError(t, s.SetPendingSwitchLatest(true))
	s2, err := Load(p)
	assert.NoError(t, err)
	assert.False(t, s2.AbsentAtLoad())

	assert.NoError(t, os.WriteFile(p, []byte("{"), 0644))
	s3, err := Load(p)
	assert.Error(t, err)
	assert.False(t, s3.AbsentAtLoad())
	assert.NotNil(t, s3.MarksLostAt())
}

// The excluded heads are persisted in one write, survive a reload, and
// leave the file at version 1. An empty list writes nothing. Mutant:
// ExcludeHeads commits for an empty list.
func TestExcludeHeadsPersistsAndEmptyWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.NoError(t, s.ExcludeHeads(nil))
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err), "no heads, no write")

	b := Head{Remote: "origin", Branch: "testing-r1", Commit: "b"}
	a := Head{Remote: "origin", Branch: "testing-r1", Commit: "a"}
	assert.NoError(t, s.ExcludeHeads([]Head{b, a, b}))
	assert.Equal(t, []Head{a, b}, s.ExcludedHeads())

	s2, err := Load(p)
	assert.NoError(t, err)
	assert.Equal(t, []Head{a, b}, s2.ExcludedHeads())
	assert.False(t, s2.AbsentAtLoad())
	assert.Nil(t, s2.MarksLostAt())

	raw, err := os.ReadFile(p)
	assert.NoError(t, err)
	var d map[string]any
	assert.NoError(t, json.Unmarshal(raw, &d))
	assert.EqualValues(t, 1, d["version"])

	// Recorded again, nothing changes on disk.
	before, err := os.Stat(p)
	assert.NoError(t, err)
	assert.NoError(t, s2.ExcludeHeads([]Head{a}))
	after, err := os.Stat(p)
	assert.NoError(t, err)
	assert.True(t, os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()))
}

// The field is additive: a file without exclusions has none of it (the
// shape the base writes), and a reader that predates it (decoding into
// the base's struct) loads a file that has it. Mutant: the field is not
// omitempty.
func TestExcludedTestingHeadsFieldIsAdditive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease-state.json")
	s, err := Load(p)
	assert.NoError(t, err)
	assert.NoError(t, s.MarkLeaseAware("d1"))
	raw, err := os.ReadFile(p)
	assert.NoError(t, err)
	assert.NotContains(t, string(raw), "excluded_testing_heads")

	assert.NoError(t, s.ExcludeHeads([]Head{{Remote: "origin", Branch: "testing-r1", Commit: "a"}}))
	raw, err = os.ReadFile(p)
	assert.NoError(t, err)
	assert.Contains(t, string(raw), "excluded_testing_heads")
	var base struct {
		Version               int             `json:"version"`
		LeaseAwareDeployments map[string]bool `json:"lease_aware_deployments"`
		ReleasedDeployments   map[string]bool `json:"released_deployments"`
		DriftSince            *time.Time      `json:"drift_since"`
		PendingSwitchLatest   bool            `json:"pending_switch_latest"`
		MarksLostAt           *time.Time      `json:"marks_lost_at"`
	}
	assert.NoError(t, json.Unmarshal(raw, &base))
	assert.Equal(t, 1, base.Version)
	assert.True(t, base.LeaseAwareDeployments["d1"])
}
