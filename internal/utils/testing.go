package utils

import (
	"context"
	"fmt"

	"github.com/nlewo/comin/internal/protobuf"
)

type RepositoryMock struct {
	RsCh chan *protobuf.RepositoryStatus
}

func NewRepositoryMock() (r *RepositoryMock) {
	rsCh := make(chan *protobuf.RepositoryStatus, 5)
	return &RepositoryMock{
		RsCh: rsCh,
	}
}
func (r *RepositoryMock) FetchAndUpdate(ctx context.Context, remoteNames []string) (rsCh chan *protobuf.RepositoryStatus) {
	return r.RsCh
}
func (r *RepositoryMock) GetRepositoryStatus() *protobuf.RepositoryStatus {
	return &protobuf.RepositoryStatus{}
}

// IsAncestor fails: the mock has no commit history.
func (r *RepositoryMock) IsAncestor(base, top string) (bool, error) {
	return false, fmt.Errorf("RepositoryMock: no commit history")
}
