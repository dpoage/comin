package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The preflight sees the commit message without its trailing newlines, and
// with everything before them kept.
func TestPreflightGitMsgTrailingNewlinesAreTrimmed(t *testing.T) {
	r := newRig(t, rigOpts{preflight: true})
	testing := r.commitMsg("testing", "file-msg", "subject\n\nbody\n\n")
	r.labels[testing] = "testing"
	r.start()
	r.waitDeploys(1)
	assert.Equal(t, r.out(testing), r.exec.deployLog()[0].out)
	assert.Equal(t, "subject\n\nbody", r.gitMsg(testing))
}
