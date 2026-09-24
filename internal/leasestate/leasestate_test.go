package leasestate

import (
	"path/filepath"
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
