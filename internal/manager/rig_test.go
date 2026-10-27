package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/internal/deployer"
	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/prometheus"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/repository"
	"github.com/nlewo/comin/internal/scheduler"
	"github.com/nlewo/comin/internal/store"
	"github.com/nlewo/comin/internal/types"
	"github.com/stretchr/testify/require"
)

// The rig wires a real go-git fixture repository, the real repository,
// fetcher, builder, deployer, store and preflight runner to a fake
// executor whose Deploy moves the running system and can be told to fail.

type deployCall struct {
	out, operation string
	// asks is the number of preflight runs logged when Deploy was called.
	asks int
}

type fakeExec struct {
	mu          sync.Mutex
	storeDir    string
	currentLink string
	askLog      string
	evals       []string
	deploys     []deployCall
	failDeploy  bool
	failMoves   bool
	restart     bool
	reboot      bool
	unknown     bool
	// buildDelay is how long a build takes unless cancelled; builds counts the started ones.
	buildDelay time.Duration
	builds     int
}

func (e *fakeExec) Eval(_ context.Context, _, _, commitId, _, _ string) (string, string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evals = append(e.evals, commitId)
	return filepath.Join(e.storeDir, commitId+".drv"), filepath.Join(e.storeDir, commitId), "", nil
}

func (e *fakeExec) Build(ctx context.Context, drvPath string) error {
	e.mu.Lock()
	e.builds++
	delay := e.buildDelay
	e.mu.Unlock()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return os.MkdirAll(strings.TrimSuffix(drvPath, ".drv"), 0o755)
}

func (e *fakeExec) buildCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.builds
}

func (e *fakeExec) Deploy(_ context.Context, out, operation string) (bool, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deploys = append(e.deploys, deployCall{out: out, operation: operation, asks: countLines(e.askLog)})
	if e.failDeploy {
		if e.failMoves {
			e.setCurrentLocked(out)
		}
		return e.restart, "", errors.New("switch failed")
	}
	e.setCurrentLocked(out)
	return e.restart, "profile", nil
}

func (e *fakeExec) NeedToReboot(_, _ string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reboot
}
func (e *fakeExec) ReadMachineId() (string, error) { return "", nil }
func (e *fakeExec) IsStorePathExist(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (e *fakeExec) CurrentSystem() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unknown {
		return "", errors.New("unknown")
	}
	return os.Readlink(e.currentLink)
}

func (e *fakeExec) setCurrentLocked(p string) {
	_ = os.Remove(e.currentLink)
	if err := os.Symlink(p, e.currentLink); err != nil {
		panic(err)
	}
}

func (e *fakeExec) setCurrent(p string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setCurrentLocked(p)
}

func (e *fakeExec) set(f func(*fakeExec)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(e)
}

func (e *fakeExec) evalLog() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.evals...)
}

func (e *fakeExec) deployLog() []deployCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]deployCall(nil), e.deploys...)
}

func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

type ask struct {
	branch, out, sha, ref, msg, operation, current, trigger, hostname string
	outExists                                                         bool
}

type rigOpts struct {
	preflight bool
	// manual and autoDelay select the deploy confirmer mode; the default confirms at once.
	manual    bool
	autoDelay time.Duration
	tick      time.Duration
}

type rig struct {
	t         *testing.T
	opts      rigOpts
	root      string
	stateDir  string
	remote    *git.Repository
	remoteDir string
	scriptDir string
	exec      *fakeExec
	exits     chan int
	cancel    context.CancelFunc
	m         *Manager
	labels    map[string]string
}

const oldSystem = "/nix/store/old-system"

func newRig(t *testing.T, opts rigOpts) *rig {
	t.Helper()
	if opts.tick == 0 {
		opts.tick = 100 * time.Millisecond
	}
	root := t.TempDir()
	r := &rig{
		t:         t,
		opts:      opts,
		root:      root,
		stateDir:  filepath.Join(root, "state"),
		remoteDir: filepath.Join(root, "remote"),
		scriptDir: filepath.Join(root, "script"),
		exits:     make(chan int, 4),
		labels:    map[string]string{},
	}
	fake := filepath.Join(root, "fake")
	for _, d := range []string{r.stateDir, filepath.Join(r.stateDir, "gcroots"), r.remoteDir, r.scriptDir, filepath.Join(fake, "store")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	r.exec = &fakeExec{
		storeDir:    filepath.Join(fake, "store"),
		currentLink: filepath.Join(fake, "current"),
		askLog:      filepath.Join(r.scriptDir, "asks.log"),
	}
	r.exec.setCurrent(oldSystem)
	r.writeScript()
	r.initRemote()
	return r
}

func (r *rig) writeScript() {
	body := fmt.Sprintf(`#!/bin/sh
d=%q
exists=no
[ -e "$COMIN_OUT_PATH" ] && exists=yes
printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s\n' "$COMIN_BRANCH" "$COMIN_OUT_PATH" "$COMIN_GIT_SHA" "$COMIN_GIT_REF" "$COMIN_GIT_MSG" "$COMIN_OPERATION" "$COMIN_CURRENT_SYSTEM" "$COMIN_TRIGGER" "$COMIN_HOSTNAME" "$exists" >> "$d/asks.log"
if [ -e "$d/block" ]; then sleep 10; fi
if [ -e "$d/slow" ]; then
  read secs from < "$d/slow"
  [ "$(wc -l < "$d/asks.log")" -ge "$from" ] && sleep "$secs"
fi
v=$(cat "$d/verdict-$COMIN_BRANCH" 2>/dev/null)
if [ "$v" = refuse ]; then echo "refused: $COMIN_BRANCH"; exit 1; fi
exit 0
`, r.scriptDir)
	require.NoError(r.t, os.WriteFile(filepath.Join(r.scriptDir, "preflight.sh"), []byte(body), 0o755))
}

func (r *rig) verdict(branch, v string) {
	r.t.Helper()
	tmp := filepath.Join(r.scriptDir, "tmp-"+branch)
	require.NoError(r.t, os.WriteFile(tmp, []byte(v), 0o644))
	require.NoError(r.t, os.Rename(tmp, filepath.Join(r.scriptDir, "verdict-"+branch)))
}

func (r *rig) block(on bool) {
	r.t.Helper()
	p := filepath.Join(r.scriptDir, "block")
	if on {
		require.NoError(r.t, os.WriteFile(p, nil, 0o644))
	} else {
		_ = os.Remove(p)
	}
}

// slowAsks makes the asks from the from-th logged one on take d; d == 0
// makes them fast again.
func (r *rig) slowAsks(d time.Duration, from int) {
	r.t.Helper()
	p := filepath.Join(r.scriptDir, "slow")
	if d == 0 {
		_ = os.Remove(p)
		return
	}
	tmp := p + ".tmp"
	require.NoError(r.t, os.WriteFile(tmp, fmt.Appendf(nil, "%g %d\n", d.Seconds(), from), 0o644))
	require.NoError(r.t, os.Rename(tmp, p))
}

func (r *rig) asks() []ask {
	b, _ := os.ReadFile(r.exec.askLog)
	var out []ask
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 10 {
			continue
		}
		out = append(out, ask{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9] == "yes"})
	}
	return out
}

func (r *rig) asksFor(out string) int {
	n := 0
	for _, a := range r.asks() {
		if a.out == out {
			n++
		}
	}
	return n
}

func (r *rig) commit(branch, content string) string {
	r.t.Helper()
	w, err := r.remote.Worktree()
	require.NoError(r.t, err)
	_ = w.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Force: true})
	require.NoError(r.t, os.WriteFile(filepath.Join(r.remoteDir, content), []byte(content), 0o644))
	_, err = w.Add(content)
	require.NoError(r.t, err)
	hash, err := w.Commit(content, &git.CommitOptions{Author: &object.Signature{Name: "John Doe", Email: "john@doe.org", When: time.Unix(0, 0)}})
	require.NoError(r.t, err)
	return hash.String()
}

func (r *rig) initRemote() {
	var err error
	r.remote, err = git.PlainInit(r.remoteDir, false)
	require.NoError(r.t, err)
	r.commit("main", "file-1")
	head := r.commit("main", "file-2")
	for _, b := range []string{"main", "testing"} {
		require.NoError(r.t, r.remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), plumbing.NewHash(head))))
	}
	r.labels[head] = "main"
}

// pushTesting commits on top of main in the testing branch.
func (r *rig) pushTesting(name string) string {
	c := r.commit("testing", name)
	r.labels[c] = "testing"
	return c
}

// pushMain commits on the main branch; the testing branch is reset to it.
func (r *rig) pushMain(name string) string {
	c := r.commit("main", name)
	require.NoError(r.t, r.remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("testing"), plumbing.NewHash(c))))
	r.labels[c] = name
	return c
}

func (r *rig) out(commit string) string { return filepath.Join(r.exec.storeDir, commit) }

func (r *rig) start() {
	t := r.t
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r.cancel = cancel
	bk := broker.New()
	bk.Start()
	st, err := store.New(bk, filepath.Join(r.stateDir, "store.json"), filepath.Join(r.stateDir, "gcroots"), 10, 10)
	require.NoError(t, err)
	require.NoError(t, st.Load())

	mainCommitId := ""
	var last *protobuf.Deployment
	if ok, ld := st.LastDeployment(); ok {
		mainCommitId, last = ld.Generation.MainCommitId, ld
	}
	gitConfig := types.GitConfig{
		Path: filepath.Join(r.stateDir, "repository"),
		Remotes: []types.Remote{{
			Name:     "origin",
			URL:      r.remoteDir,
			Branches: types.Branches{Main: types.Branch{Name: "main"}, Testing: types.Branch{Name: "testing"}},
			Timeout:  30,
		}},
	}
	repo, err := repository.New(gitConfig, mainCommitId, prometheus.New())
	require.NoError(t, err)
	f := fetcher.NewFetcher(repo)
	f.Start(ctx)
	b := builder.New(st, r.exec, gitConfig.Path, "", "", "my-machine", time.Minute, time.Minute)
	d := deployer.New(st, r.exec.Deploy, last, "")
	bc := NewConfirmer(bk, Without, 0, "build")
	bc.Start()
	mode := Without
	if r.opts.manual {
		mode = Manual
	} else if r.opts.autoDelay > 0 {
		mode = Auto
	}
	dc := NewConfirmer(bk, mode, r.opts.autoDelay, "deploy")
	dc.Start()
	var cmd preflight.Command
	if r.opts.preflight {
		cmd = preflight.Command{Path: filepath.Join(r.scriptDir, "preflight.sh"), Timeout: 30 * time.Second}
	}
	ops := ConfigurationOperations{"origin": {"main": "switch", "testing": "test"}}
	r.m = New(st, prometheus.New(), scheduler.New(), f, b, d, "", r.exec, bc, dc, bk, ops, cmd)
	r.m.tick = r.opts.tick
	r.m.exit = func(code int) { r.exits <- code }
	go r.m.Run(ctx)
	r.fetch()
	// The state directory is removed after the fetch worker, which ignores ctx, is done.
	t.Cleanup(func() {
		cancel()
		for range 100 {
			if !f.IsFetching() {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	})
}

func (r *rig) stop() {
	r.cancel()
	time.Sleep(200 * time.Millisecond)
}

func (r *rig) fetch() { r.m.Fetcher.TriggerFetch([]string{"origin"}) }

func (r *rig) waitDeploys(n int) {
	r.t.Helper()
	require.Eventually(r.t, func() bool { return len(r.exec.deployLog()) >= n }, 15*time.Second, 25*time.Millisecond, "deployments: %v", r.exec.deployLog())
}

func (r *rig) waitCurrent(p string) {
	r.t.Helper()
	require.Eventually(r.t, func() bool {
		c, _ := r.exec.CurrentSystem()
		return c == p
	}, 15*time.Second, 25*time.Millisecond, "current system")
}

// settle lets n ticks pass.
func (r *rig) settle(n int) { time.Sleep(time.Duration(n) * r.opts.tick) }

func (r *rig) evalLabels() []string {
	var out []string
	for _, c := range r.exec.evalLog() {
		out = append(out, r.labels[c])
	}
	return out
}
