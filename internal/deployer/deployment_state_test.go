package deployer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
)

// writeBlockingPostDeploymentCommand writes a post-deployment command that
// creates started, then blocks until release exists; it returns script and dir.
func writeBlockingPostDeploymentCommand(t *testing.T) (script, dir string) {
	t.Helper()
	dir = t.TempDir()
	script = filepath.Join(dir, "post-deployment-command")
	content := "#!/bin/sh\n" +
		": > " + filepath.Join(dir, "started") + "\n" +
		"while [ ! -e " + filepath.Join(dir, "release") + " ]; do sleep 0.05; done\n"
	assert.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	t.Cleanup(func() {
		// Unblock the command so the deployer goroutine is never leaked.
		_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o644)
	})
	return script, dir
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// While the deploy func runs, comin status reports the store's started copy.
func TestDeployerShowsTheRunningDeploymentWhileDeploying(t *testing.T) {
	deployFuncStarted := make(chan struct{})
	deployFuncDone := make(chan struct{})
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		deployFuncStarted <- struct{}{}
		<-deployFuncDone
		return false, "profile-path", nil
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
	case <-deployFuncStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the deploy func never started")
	}
	dpl := d.State().Deployment
	assert.Equal(t, store.StatusToString(store.Running), dpl.Status)
	assert.NotNil(t, dpl.StartedAt)

	close(deployFuncDone)
	select {
	case <-d.DeploymentDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the deployer did not announce the finished deployment")
	}
}

// While the post-deployment command runs, comin status reports the store's
// finished copy, not a deployment still running.
func TestDeployerShowsTheFinishedDeploymentWhileThePostDeploymentCommandRuns(t *testing.T) {
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		return false, "profile-path", nil
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	script, dir := writeBlockingPostDeploymentCommand(t)
	d := deployer.New(s, deployFunc, nil, script)
	d.Run(t.Context())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "switch")

	waitForFile(t, filepath.Join(dir, "started"))
	dpl := d.State().Deployment
	assert.Equal(t, store.StatusToString(store.Done), dpl.Status)
	assert.NotNil(t, dpl.EndedAt)
	assert.Equal(t, "profile-path", dpl.ProfilePath)
	assert.Equal(t, "", dpl.ErrorMsg)
	assert.Equal(t, store.StatusToString(store.Done), d.Deployment().Status)

	assert.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0o644))
	select {
	case <-d.DeploymentDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the deployer did not announce the finished deployment")
	}
}

// The same holds when the deploy func errors: the failed deployment is shown.
func TestDeployerShowsTheFailedDeploymentWhileThePostDeploymentCommandRuns(t *testing.T) {
	deployFunc := func(context.Context, string, string) (bool, string, error) {
		return false, "", errors.New("switch-to-configuration failed")
	}

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := store.New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)
	script, dir := writeBlockingPostDeploymentCommand(t)
	d := deployer.New(s, deployFunc, nil, script)
	d.Run(t.Context())

	d.Submit(&protobuf.Generation{SelectedCommitId: "commit-1"}, "switch")

	waitForFile(t, filepath.Join(dir, "started"))
	dpl := d.State().Deployment
	assert.Equal(t, store.StatusToString(store.Failed), dpl.Status)
	assert.NotNil(t, dpl.EndedAt)
	assert.Equal(t, "switch-to-configuration failed", dpl.ErrorMsg)
	assert.Equal(t, "", dpl.ProfilePath)
	assert.Equal(t, store.StatusToString(store.Failed), d.Deployment().Status)

	assert.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0o644))
	select {
	case <-d.DeploymentDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the deployer did not announce the finished deployment")
	}
}
