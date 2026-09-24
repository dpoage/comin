// Package leasestate persists the cross-restart bookkeeping the fork needs
// for lease-aware deployment decisions, kept in a file of its own so it
// never touches store.json's schema (store.json must stay loadable by a
// pre-fork comin, whose store.Load rejects unknown fields).
//
// It hides: the on-disk path and JSON shape of that file, the fact that
// every mutation is written back to disk immediately (so a restart never
// loses a mark), and the difference between "no drift observed yet" and
// "drift observed, cleared".
package leasestate

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type data struct {
	Version               int             `json:"version"`
	LeaseAwareDeployments map[string]bool `json:"lease_aware_deployments"`
	ReleasedDeployments   map[string]bool `json:"released_deployments"`
	DriftSince            *time.Time      `json:"drift_since"`
	PendingSwitchLatest   bool            `json:"pending_switch_latest"`
}

// State is comin's own persisted state for the override-lease feature. All
// methods are safe for concurrent use and durable: a successful mutation is
// on disk before the method returns.
type State struct {
	mu       sync.Mutex
	filename string
	d        data
}

// Load reads the state file at filename, creating an empty one in memory if
// it does not exist yet (nothing is written to disk until the first
// mutation). A malformed file is treated the same as a missing one: this is
// comin's own best-effort bookkeeping, not a durability-critical source of
// truth like store.json.
func Load(filename string) (*State, error) {
	s := &State{
		filename: filename,
		d: data{
			Version:               1,
			LeaseAwareDeployments: map[string]bool{},
			ReleasedDeployments:   map[string]bool{},
		},
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, nil
	}
	var d data
	if err := json.Unmarshal(content, &d); err != nil {
		return s, nil
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

// commit writes the current state to disk. Callers hold s.mu.
func (s *State) commit() error {
	buf, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filename, buf, 0644)
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
