// Package leasestate persists the cross-restart bookkeeping the fork needs
// for lease-aware deployment decisions, kept in a file of its own so it
// never touches store.json's schema (store.json must stay loadable by a
// pre-fork comin, whose store.Load rejects unknown fields).
//
// It hides: the JSON shape of that file, the fact that every mutation is
// written back to disk atomically before it returns (so a restart never
// loses a mark), and the difference between "no drift observed yet" and
// "drift observed, cleared". The caller chooses the file's path.
package leasestate

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const version = 1

type data struct {
	Version               int             `json:"version"`
	LeaseAwareDeployments map[string]bool `json:"lease_aware_deployments"`
	ReleasedDeployments   map[string]bool `json:"released_deployments"`
	DriftSince            *time.Time      `json:"drift_since"`
	PendingSwitchLatest   bool            `json:"pending_switch_latest"`
	MarksLostAt           *time.Time      `json:"marks_lost_at"`
	// ExcludedTestingHeads is additive and omitted when empty, so the
	// file stays version 1: a comin that predates it loads the file and
	// drops the field on its next write.
	ExcludedTestingHeads []Head `json:"excluded_testing_heads,omitempty"`
}

// Head names the head of a testing branch. It has the fields of
// repository.TestingHead, so the two convert directly.
type Head struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// State is comin's own persisted state for the override-lease feature. All
// methods are safe for concurrent use and durable: a successful mutation is
// on disk before the method returns, and a crash at any point leaves
// either the previous or the new state on disk, never a partial one.
type State struct {
	mu       sync.Mutex
	filename string
	d        data
	// absent is set when Load found no file. It is a fact about the
	// start, not persisted, and never changes afterwards.
	absent bool
}

// Load reads the state file at filename. It always returns a usable
// State. A missing file yields an empty State, a nil error and
// AbsentAtLoad() == true (nothing is written until the first mutation).
// A file that exists but cannot be read or parsed yields an empty State
// AND a non-nil error: the caller must report it. Every mark the file
// held is lost, so the State records the moment of the loss (MarksLostAt)
// and writes it back at once.
func Load(filename string) (*State, error) {
	s := &State{
		filename: filename,
		d: data{
			Version:               version,
			LeaseAwareDeployments: map[string]bool{},
			ReleasedDeployments:   map[string]bool{},
		},
	}
	content, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		s.absent = true
		return s, nil
	}
	if err != nil {
		err = fmt.Errorf("leasestate: cannot read %s, starting without lease-aware marks: %w", filename, err)
	} else {
		var d data
		if uerr := json.Unmarshal(content, &d); uerr != nil {
			err = fmt.Errorf("leasestate: %s is corrupt (%d bytes), starting without lease-aware marks: %w", filename, len(content), uerr)
		} else if d.Version != version {
			err = fmt.Errorf("leasestate: %s has version %d, want %d; starting without lease-aware marks", filename, d.Version, version)
		} else {
			if d.LeaseAwareDeployments == nil {
				d.LeaseAwareDeployments = map[string]bool{}
			}
			if d.ReleasedDeployments == nil {
				d.ReleasedDeployments = map[string]bool{}
			}
			s.d = d
			return s, nil
		}
	}
	lost := time.Now().UTC()
	s.d.MarksLostAt = &lost
	return s, errors.Join(err, s.commit())
}

// MarksLostAt returns when the marks were last lost to an unreadable
// state file, or nil if they never were. A deployment that ended before
// that moment may have carried marks this State no longer has.
func (s *State) MarksLostAt() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.MarksLostAt == nil {
		return nil
	}
	t := *s.d.MarksLostAt
	return &t
}

// AbsentAtLoad reports whether Load found no state file: comin never
// wrote one, or it was moved aside. The marks it would have held are
// unknown, but no loss is recorded (MarksLostAt stays nil) and nothing is
// written until a mutation.
func (s *State) AbsentAtLoad() bool {
	return s.absent
}

// ExcludeHeads records, in one write, that the testing heads must never
// be selected or deployed. An empty list writes nothing. Heads already
// recorded are kept; the list stays sorted so the file is deterministic.
func (s *State) ExcludeHeads(heads []Head) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := false
	for _, h := range heads {
		if !slices.Contains(s.d.ExcludedTestingHeads, h) {
			s.d.ExcludedTestingHeads = append(s.d.ExcludedTestingHeads, h)
			added = true
		}
	}
	if !added {
		return nil
	}
	slices.SortFunc(s.d.ExcludedTestingHeads, func(a, b Head) int {
		return cmp.Or(strings.Compare(a.Remote, b.Remote), strings.Compare(a.Branch, b.Branch), strings.Compare(a.Commit, b.Commit))
	})
	return s.commit()
}

// ExcludedHeads returns the heads recorded by ExcludeHeads.
func (s *State) ExcludedHeads() []Head {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.ExcludedTestingHeads)
}

// commit writes the current state to disk: a temporary file in the same
// directory, fsynced, then renamed over the state file, then the directory
// fsynced. Callers hold s.mu.
func (s *State) commit() error {
	buf, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.filename)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.filename)+".tmp-*")
	if err != nil {
		return err
	}
	// Best-effort: after a successful rename the path no longer exists.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.filename); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// MarkLeaseAware records that uuid is a deployment made by lease-aware
// comin. Only deployments with this mark are ever considered for release
// (C3): a deployment made by a pre-fork comin is never released, so
// upgrading over an in-progress testing deployment cannot loop.
func (s *State) MarkLeaseAware(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.LeaseAwareDeployments[uuid] {
		return nil
	}
	s.d.LeaseAwareDeployments[uuid] = true
	return s.commit()
}

// IsLeaseAware reports whether uuid was previously marked by MarkLeaseAware.
func (s *State) IsLeaseAware(uuid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.LeaseAwareDeployments[uuid]
}

// Release records, in one write, that the deployments uuids belong to an
// override that ended and must never again be treated as live. A crash
// leaves either none of them released on disk or all of them.
func (s *State) Release(uuids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, uuid := range uuids {
		s.d.ReleasedDeployments[uuid] = true
	}
	return s.commit()
}

// IsReleased reports whether uuid was recorded by Release.
func (s *State) IsReleased(uuid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.ReleasedDeployments[uuid]
}

// Forget drops every mark recorded for uuid. The manager calls it once a
// deployment has left the store, so the marks do not grow without bound.
func (s *State) Forget(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.d.LeaseAwareDeployments[uuid] && !s.d.ReleasedDeployments[uuid] {
		return nil
	}
	delete(s.d.LeaseAwareDeployments, uuid)
	delete(s.d.ReleasedDeployments, uuid)
	return s.commit()
}

// DriftSince returns the start of the current drift episode, or nil when
// none is recorded.
func (s *State) DriftSince() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.DriftSince == nil {
		return nil
	}
	t := *s.d.DriftSince
	return &t
}

// SetDriftSince persists the start of the current drift episode. Passing
// nil clears it (the machine is back to its expected state).
func (s *State) SetDriftSince(since *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if since == nil {
		if s.d.DriftSince == nil {
			return nil
		}
		s.d.DriftSince = nil
		return s.commit()
	}
	if s.d.DriftSince != nil && s.d.DriftSince.Equal(*since) {
		return nil
	}
	t := *since
	s.d.DriftSince = &t
	return s.commit()
}

// PendingSwitchLatest reports whether a `comin deployment switch-latest`
// request is still outstanding: requested, but not yet resolved and
// deployed. This survives a comin restart between the request and the
// deployer actually picking it up.
func (s *State) PendingSwitchLatest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.PendingSwitchLatest
}

// SetPendingSwitchLatest records or clears the outstanding switch-latest
// request.
func (s *State) SetPendingSwitchLatest(v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.PendingSwitchLatest == v {
		return nil
	}
	s.d.PendingSwitchLatest = v
	return s.commit()
}
