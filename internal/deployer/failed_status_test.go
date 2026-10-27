package deployer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
)

// A deploy func error is recorded failed in the store and in the announced deployment.
func TestDeployerRecordsAFailedDeployment(t *testing.T) {
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		return false, "", errors.New("switch-to-configuration failed")
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "switch")

	select {
	case dpl := <-d.DeploymentDoneCh:
		assert.Equal(t, store.StatusToString(store.Failed), dpl.Status)
		assert.Equal(t, "switch-to-configuration failed", dpl.ErrorMsg)
		assert.NotNil(t, dpl.EndedAt)
	case <-time.After(5 * time.Second):
		t.Fatal("the deployer did not announce the finished deployment")
	}

	stored := s.DeploymentList()
	assert.Len(t, stored, 1)
	assert.Equal(t, store.StatusToString(store.Failed), stored[0].Status)
	assert.Equal(t, "switch-to-configuration failed", stored[0].ErrorMsg)
}

// The announced deployment is the store's finished copy.
func TestDeployerAnnouncesTheFinishedDeployment(t *testing.T) {
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		return true, "profile-path", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	d := deployer.New(s, deployFunc, nil, "")
	d.Run(t.Context())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "switch")

	select {
	case dpl := <-d.DeploymentDoneCh:
		assert.Equal(t, store.StatusToString(store.Done), dpl.Status)
		assert.Equal(t, "profile-path", dpl.ProfilePath)
		assert.True(t, dpl.RestartComin.GetValue())
		assert.NotNil(t, dpl.EndedAt)
	case <-time.After(5 * time.Second):
		t.Fatal("the deployer did not announce the finished deployment")
	}
	assert.Equal(t, store.StatusToString(store.Done), s.DeploymentList()[0].Status)
}
