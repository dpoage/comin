package utils

import (
	"context"
	"fmt"

	"github.com/nlewo/comin/internal/protobuf"
)

type RepositoryMock struct {
	RsCh chan *protobuf.RepositoryStatus
	// IsAncestorFunc answers IsAncestor. Nil makes IsAncestor fail.
	IsAncestorFunc func(base, top string) (bool, error)
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
func (r *RepositoryMock) IsAncestor(base, top string) (bool, error) {
	if r.IsAncestorFunc == nil {
		return false, fmt.Errorf("RepositoryMock: IsAncestor is not configured")
	}
	return r.IsAncestorFunc(base, top)
}
