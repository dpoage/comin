package store

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nlewo/comin/internal/protobuf"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Status int64

const (
	Init Status = iota
	Running
	Done
	Failed
)

func StatusToString(status Status) string {
	switch status {
	case Init:
		return "init"
	case Running:
		return "running"
	case Done:
		return "done"
	case Failed:
		return "failed"
	}
	return ""
}

func StringToStatus(statusStr string) Status {
	switch statusStr {
	case "init":
		return Init
	case "running":
		return Running
	case "done":
		return Done
	case "failed":
		return Failed
	}
	return Init
}

func IsTesting(d *protobuf.Deployment) bool {
	return d.Operation == "test"
}

// NewDeployment records a new deployment and returns a copy of it. The
// store keeps its own row: later transitions (DeploymentStarted,
// DeploymentFinished) return fresh copies rather than mutating a value a
// caller holds.
func (s *Store) NewDeployment(g *protobuf.Generation, operation, reason string) *protobuf.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &protobuf.Deployment{
		Uuid:       uuid.New().String(),
		Generation: g,
		Operation:  operation,
		Reason:     reason,
		Status:     StatusToString(Init),
	}
	s.data.Deployments = append(s.data.Deployments, d)
	return proto.CloneOf(d)
}

func (s *Store) deploymentGet(uuid string) (g *protobuf.Deployment, err error) {
	for _, d := range s.data.Deployments {
		if d.Uuid == uuid {
			return d, nil
		}
	}
	return nil, fmt.Errorf("store: no deployment with uuid %s has been found", uuid)
}

// DeploymentStarted marks the deployment running and returns a copy of it.
func (s *Store) DeploymentStarted(uuid string) (*protobuf.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deploymentGet(uuid)
	if err != nil {
		return nil, err
	}
	d.StartedAt = timestamppb.New(time.Now().UTC())
	d.Status = StatusToString(Running)
	started := proto.CloneOf(d)
	e := &protobuf.Event_DeploymentStarted{Deployment: proto.CloneOf(d)}
	s.broker.Publish(&protobuf.Event{Type: &protobuf.Event_DeploymentStartedType{DeploymentStartedType: e}})
	return started, nil
}

// DeploymentFinished marks the deployment done or failed and returns a copy
// of it.
func (s *Store) DeploymentFinished(uuid string, deploymentErr error, cominNeedRestart bool, profilePath string) (*protobuf.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deploymentGet(uuid)
	if err != nil {
		return nil, err
	}
	if deploymentErr != nil {
		d.ErrorMsg = deploymentErr.Error()
		d.Status = StatusToString(Failed)
	} else {
		d.Status = StatusToString(Done)
	}
	d.EndedAt = timestamppb.New(time.Now().UTC())
	d.RestartComin = wrapperspb.Bool(cominNeedRestart)
	d.ProfilePath = profilePath
	finished := proto.CloneOf(d)
	e := &protobuf.Event_DeploymentFinished{Deployment: proto.CloneOf(d)}
	s.broker.Publish(&protobuf.Event{Type: &protobuf.Event_DeploymentFinishedType{DeploymentFinishedType: e}})
	return finished, nil
}
