package manager

import (
	"github.com/nlewo/comin/internal/predeploy"
	"github.com/nlewo/comin/internal/protobuf"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// preDeployState reports the stage in the State: a nil stage (no
// pre-deployment command) reports an empty message, never nil.
func preDeployState(s *predeploy.Stage) *protobuf.PreDeploy {
	if s == nil {
		return &protobuf.PreDeploy{}
	}
	st := s.Status()
	return &protobuf.PreDeploy{
		Running: preDeployEntry(st.Running),
		Pending: preDeployEntry(st.Pending),
	}
}

// preDeployEntry leaves started_at unset for a pending entry.
func preDeployEntry(e *predeploy.Entry) *protobuf.PreDeployEntry {
	if e == nil {
		return nil
	}
	out := &protobuf.PreDeployEntry{Generation: e.Generation, Operation: e.Operation}
	if !e.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(e.StartedAt)
	}
	return out
}
