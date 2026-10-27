// Package lease reads the developer override lease file that lives outside
// comin's control (owned by the platform's `override-lease` tool). comin is
// deliberately restricted to two facts about that file: whether it exists,
// and its top-level "kind". It never parses expiry, target, or any other
// field, and it never treats a missing, unreadable, or unparseable "kind" as
// anything but a non-git lease.
package lease

import (
	"encoding/json"
	"os"
)

// Kind is the lease's declared kind, as observed by comin. The zero value
// means "present but not confirmed git" (missing, unreadable, or
// unparseable), which comin treats the same as session/closure for gating
// purposes.
type Kind string

const (
	KindGit     Kind = "git"
	KindClosure Kind = "closure"
	KindSession Kind = "session"
)

// Observation is what comin knows about the lease file at one point in time.
type Observation struct {
	// Exists is true when the lease file is present, regardless of
	// whether its content could be read or parsed.
	Exists bool
	// Kind is the lease's kind when Exists is true and the "kind" field
	// could be read and recognized; the zero value otherwise.
	Kind Kind
}

// IsGit reports whether the lease is confirmed present with kind "git".
func (o Observation) IsGit() bool {
	return o.Exists && o.Kind == KindGit
}

// Reader observes the override lease file at a fixed path. A Reader with an
// empty path is disabled: Observe always reports no lease.
type Reader struct {
	path string
}

// NewReader returns a Reader for the lease file at path. An empty path
// disables lease awareness entirely.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Enabled reports whether this Reader was configured with a lease file path.
func (r *Reader) Enabled() bool {
	return r.path != ""
}

// Observe reads the lease file's existence and kind fresh from disk. It
// never returns an error: every failure mode (disabled reader, missing
// file, unreadable file, malformed JSON, missing or unrecognized "kind")
// collapses to a well-defined Observation.
func (r *Reader) Observe() Observation {
	if r.path == "" {
		return Observation{}
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Observation{}
		}
		// The file exists but could not be read (e.g. a permission
		// error): treat it as present with an unrecognized kind.
		return Observation{Exists: true}
	}
	var rec struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Observation{Exists: true}
	}
	switch Kind(rec.Kind) {
	case KindGit, KindClosure, KindSession:
		return Observation{Exists: true, Kind: Kind(rec.Kind)}
	default:
		return Observation{Exists: true}
	}
}
