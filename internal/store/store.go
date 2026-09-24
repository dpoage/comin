package store

import (
	"errors"
	"os"
	"sync"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type State struct {
	Deployments []*protobuf.Deployment `json:"deployments"`
	Generations []*protobuf.Generation `json:"generations"`
}

type Data struct {
	Version string `json:"version"`
	// Deployments are order from the most recent to older
	Deployments []*protobuf.Deployment `json:"deployments"`
	Generations []*protobuf.Generation `json:"generations"`
}

type Store struct {
	data             *protobuf.Store
	mu               sync.Mutex
	filename         string
	generationGcRoot string
	capacityMain     int
	capacityTesting  int

	lastEvalStarted   *protobuf.Generation
	lastEvalFinished  *protobuf.Generation
	lastBuildStarted  *protobuf.Generation
	lastBuildFinished *protobuf.Generation

	broker *broker.Broker
}

func New(broker *broker.Broker, filename, gcRootsDir string, capacityMain, capacityTesting int) (*Store, error) {
	data := &protobuf.Store{
		Deployments: make([]*protobuf.Deployment, 0),
		Generations: make([]*protobuf.Generation, 0),
	}
	st := Store{
		filename:         filename,
		generationGcRoot: gcRootsDir + "/last-built-generation",
		capacityMain:     capacityMain,
		capacityTesting:  capacityTesting,
		data:             data,
		broker:           broker,
	}
	if err := os.MkdirAll(gcRootsDir, os.ModeDir); err != nil {
		return nil, err
	}
	return &st, nil
}

// GetState returns a copy of the store's data.
func (s *Store) GetState() *protobuf.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.CloneOf(s.data)
}

func (s *Store) DeploymentInsertAndCommit(dpl *protobuf.Deployment) (ok bool, evicted *protobuf.Deployment) {
	s.mu.Lock()
	ok, evicted = s.deploymentInsert(dpl)
	buf, err := s.marshal()
	s.mu.Unlock()
	if ok {
		logrus.Infof("store: the deployment %s has been removed from store.json file", evicted.Uuid)
	}
	if err == nil {
		err = os.WriteFile(s.filename, buf, 0644)
	}
	if err != nil {
		logrus.Errorf("Error while commiting the store.json file: %s", err)
		return
	}
	logrus.Infof("store: the new deployment %s has been commited to store.json file", dpl.Uuid)
	return
}

// DeploymentInsert inserts a deployment and return an evicted
// deployment because the capacity has been reached.
func (s *Store) DeploymentInsert(dpl *protobuf.Deployment) (getsEvicted bool, evicted *protobuf.Deployment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deploymentInsert(dpl)
}

func (s *Store) deploymentInsert(dpl *protobuf.Deployment) (getsEvicted bool, evicted *protobuf.Deployment) {
	var qty, older int
	capacity := s.capacityMain
	if IsTesting(dpl) {
		capacity = s.capacityTesting
	}
	for i, d := range s.data.Deployments {
		if IsTesting(dpl) == IsTesting(d) {
			older = i
			qty += 1
		}
	}
	// If the capacity is reached, we remove the older elements
	if qty >= capacity {
		evicted = s.data.Deployments[older]
		getsEvicted = true
		s.data.Deployments = append(s.data.Deployments[:older], s.data.Deployments[older+1:]...)
	}
	s.data.Deployments = append([]*protobuf.Deployment{dpl}, s.data.Deployments...)
	return
}

// DeploymentList returns a copy of the stored deployments, most recently
// inserted first. The copy is taken under the store lock, so callers can
// read it while deployments are started, finished or inserted.
func (s *Store) DeploymentList() []*protobuf.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*protobuf.Deployment, len(s.data.Deployments))
	for i, d := range s.data.Deployments {
		list[i] = proto.CloneOf(d)
	}
	return list
}

func (s *Store) LastDeployment() (ok bool, d *protobuf.Deployment) {
	if list := s.DeploymentList(); len(list) > 0 {
		return true, list[0]
	}
	return
}

func (s *Store) Load() (err error) {
	var data protobuf.Store
	content, err := os.ReadFile(s.filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return
	}
	unmarshaler := protojson.UnmarshalOptions{}
	err = unmarshaler.Unmarshal(content, &data)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.data = &data
	s.mu.Unlock()
	logrus.Infof("store: loaded %d deployments from %s", len(data.Deployments), s.filename)
	return
}

func (s *Store) Commit() error {
	s.mu.Lock()
	buf, err := s.marshal()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(s.filename, buf, 0644)
}

// marshal serializes the store's data. Callers hold s.mu.
func (s *Store) marshal() ([]byte, error) {
	marshaler := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
		AllowPartial:    true,
	}
	return marshaler.Marshal(s.data)
}
