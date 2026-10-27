package reconcile

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
)

// started is the process start the rows in this file are timed against.
var started = time.Unix(100, 0)

func TestFailedLatestAcrossARestart(t *testing.T) {
	tests := []struct {
		name string
		rows []*protobuf.Deployment
		want bool
	}{
		{"a lone failure from before the start is retried", []*protobuf.Deployment{deployment("/o", "failed", 99)}, false},
		{"a failure at the start is not retried", []*protobuf.Deployment{deployment("/o", "failed", 100)}, true},
		{"a deployment stored twice counts once", []*protobuf.Deployment{storedTwice(99), storedTwice(99), deployment("/o", "done", 97)}, false},
		{"two failures from before the start are not retried", []*protobuf.Deployment{deployment("/o", "failed", 98), deployment("/o", "failed", 99)}, true},
		{"a lone failure from before the start after a success is retried", []*protobuf.Deployment{deployment("/o", "done", 97), deployment("/o", "failed", 99)}, false},
		{"a success from before the start supersedes an older failure", []*protobuf.Deployment{deployment("/o", "failed", 98), deployment("/o", "done", 99)}, false},
		{"a failure in this process after one from before the start", []*protobuf.Deployment{deployment("/o", "failed", 99), deployment("/o", "failed", 101)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, failedLatest(tt.rows, "/o", started))
		})
	}
}

func storedTwice(endedAt int64) *protobuf.Deployment {
	d := deployment("/o", "failed", endedAt)
	d.Uuid = "twice"
	return d
}
