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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// State is comin's own persisted state for the override-lease feature. All
// methods are safe for concurrent use and durable: a successful mutation is
// on disk before the method returns, and a crash at any point leaves
// either the previous or the new state on disk, never a partial one.
type State struct {
	mu       sync.Mutex
	filename string
	d        data
}

// Load reads the state file at filename. It always returns a usable
// State. A missing file yields an empty State and a nil error (nothing is
// written until the first mutation). A file that exists but cannot be
// read or parsed yields an empty State AND a non-nil error: the caller
// must report it, because every lease-aware and released mark it held is
// lost, so no testing deployment will be released until new marks are
// recorded.
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
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("leasestate: cannot read %s, starting without lease-aware marks: %w", filename, err)
	}
	var d data
	if err := json.Unmarshal(content, &d); err != nil {
		return s, fmt.Errorf("leasestate: %s is corrupt (%d bytes), starting without lease-aware marks: %w", filename, len(content), err)
	}
	if d.Version != version {
		return s, fmt.Errorf("leasestate: %s has version %d, want %d; starting without lease-aware marks", filename, d.Version, version)
	}
	if d.LeaseAwareDeployments == nil {
		d.LeaseAwareDeployments = map[string]bool{}
	}
	if d.ReleasedDeployments == nil {
		d.ReleasedDeployments = map[string]bool{}
	}
	s.d = d
	return s, nil
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
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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
	defer d.Close()
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

// MarkReleased records that uuid is a testing deployment whose override
// ended: it is no longer "live" and must never again be treated as the
// expected deployment.
func (s *State) MarkReleased(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.ReleasedDeployments[uuid] {
		return nil
	}
	s.d.ReleasedDeployments[uuid] = true
	return s.commit()
}

// IsReleased reports whether uuid was previously marked by MarkReleased.
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
