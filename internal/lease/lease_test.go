package lease

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReaderDisabled(t *testing.T) {
	r := NewReader("")
	assert.False(t, r.Enabled())
	assert.Equal(t, Observation{}, r.Observe())
}

func TestReaderMissingFile(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "missing.json"))
	assert.True(t, r.Enabled())
	obs := r.Observe()
	assert.False(t, obs.Exists)
	assert.False(t, obs.IsGit())
}

func TestReaderKindGit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease.json")
	assert.NoError(t, os.WriteFile(p, []byte(`{"version":1,"kind":"git"}`), 0644))
	obs := NewReader(p).Observe()
	assert.True(t, obs.Exists)
	assert.Equal(t, KindGit, obs.Kind)
	assert.True(t, obs.IsGit())
}

func TestReaderKindSessionAndClosure(t *testing.T) {
	for _, kind := range []string{"session", "closure"} {
		p := filepath.Join(t.TempDir(), "lease.json")
		assert.NoError(t, os.WriteFile(p, []byte(`{"version":1,"kind":"`+kind+`"}`), 0644))
		obs := NewReader(p).Observe()
		assert.True(t, obs.Exists)
		assert.Equal(t, Kind(kind), obs.Kind)
		assert.False(t, obs.IsGit())
	}
}

func TestReaderUnparseableKindIsNonGit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease.json")
	assert.NoError(t, os.WriteFile(p, []byte(`not json`), 0644))
	obs := NewReader(p).Observe()
	assert.True(t, obs.Exists)
	assert.Equal(t, Kind(""), obs.Kind)
	assert.False(t, obs.IsGit())
}

func TestReaderMissingKindFieldIsNonGit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease.json")
	assert.NoError(t, os.WriteFile(p, []byte(`{"version":1}`), 0644))
	obs := NewReader(p).Observe()
	assert.True(t, obs.Exists)
	assert.False(t, obs.IsGit())
}

func TestReaderUnrecognizedKindIsNonGit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease.json")
	assert.NoError(t, os.WriteFile(p, []byte(`{"version":1,"kind":"bogus"}`), 0644))
	obs := NewReader(p).Observe()
	assert.True(t, obs.Exists)
	assert.False(t, obs.IsGit())
}
