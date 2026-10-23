package predeploy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/nlewo/comin/internal/protobuf"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const waitTimeout = 10 * time.Second

// blockingHook records its run, announces itself on fifo up.<uuid>, blocks on
// fifo release.<uuid> (each only when the test created it), then exits with
// the code in exit.<uuid> (default 0).
const blockingHook = `#!/bin/sh
d="$HOOKDIR"; g="$COMIN_GENERATION"
trap 'echo "$g" >> "$d/signals"' TERM INT HUP
locked=yes
mkdir "$d/lock" 2>/dev/null || { echo "$g" >> "$d/overlap"; locked=no; }
echo "$g" >> "$d/runs"
echo "$$" > "$d/pid.$g"
[ -p "$d/up.$g" ] && echo up > "$d/up.$g"
[ -p "$d/release.$g" ] && read x < "$d/release.$g"
echo done > "$d/done.$g"
[ "$locked" = yes ] && rmdir "$d/lock"
code=0
[ -f "$d/exit.$g" ] && code=$(cat "$d/exit.$g")
exit "$code"
`

type failure struct {
	g   *pb.Generation
	op  string
	err error
}

type harness struct {
	t      *testing.T
	dir    string
	stage  *Stage
	logs   *test.Hook
	events chan string

	mu        sync.Mutex
	submitted []string
	failed    []failure
}

func quietLogs(t *testing.T) *test.Hook {
	t.Helper()
	logrus.SetOutput(io.Discard)
	hook := test.NewGlobal()
	t.Cleanup(func() {
		logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
		logrus.SetOutput(os.Stderr)
	})
	return hook
}

func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "hook.sh")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o700))
	return path
}

// newHarness builds a stage running script (not started: call run).
func newHarness(t *testing.T, script string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, logs: quietLogs(t), events: make(chan string, 256)}
	t.Setenv("HOOKDIR", dir)
	h.stage = New(writeScript(t, dir, script), h.onSubmit, h.onFail)
	return h
}

func (h *harness) run(ctx context.Context) {
	h.stage.Run(ctx)
}

func (h *harness) onSubmit(g *pb.Generation, op string) {
	h.mu.Lock()
	h.submitted = append(h.submitted, g.Uuid)
	h.mu.Unlock()
	h.events <- "submit:" + g.Uuid + ":" + op
}

func (h *harness) onFail(g *pb.Generation, op string, err error) {
	h.mu.Lock()
	h.failed = append(h.failed, failure{g, op, err})
	h.mu.Unlock()
	h.events <- "fail:" + g.Uuid + ":" + op
}

func (h *harness) submittedList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.submitted...)
}

func (h *harness) failedList() []failure {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]failure{}, h.failed...)
}

func (h *harness) path(name string) string { return filepath.Join(h.dir, name) }

// fifos creates the up and release fifos for the given generations.
func (h *harness) fifos(uuids ...string) {
	for _, u := range uuids {
		require.NoError(h.t, syscall.Mkfifo(h.path("up."+u), 0o600))
		require.NoError(h.t, syscall.Mkfifo(h.path("release."+u), 0o600))
	}
	h.t.Cleanup(func() {
		for _, u := range uuids {
			f, err := os.OpenFile(h.path("release."+u), os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				_, _ = f.WriteString("x\n")
				_ = f.Close()
			}
		}
	})
}

// waitUp returns once the hook for uuid is running.
func (h *harness) waitUp(uuid string) {
	h.t.Helper()
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(h.path("up."+uuid), os.O_RDONLY, 0)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = f.Close() }()
		_, err = io.ReadAll(f)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(h.t, err)
	case <-time.After(waitTimeout):
		h.t.Fatalf("hook for %s did not start", uuid)
	}
}

// release lets the blocked hook for uuid exit with code.
func (h *harness) release(uuid string, code int) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.path("exit."+uuid), []byte(strconv.Itoa(code)), 0o600))
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(h.path("release."+uuid), os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		_, err = f.WriteString("x\n")
		_ = f.Close()
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(h.t, err)
	case <-time.After(waitTimeout):
		h.t.Fatalf("hook for %s was not blocked on its release fifo", uuid)
	}
}

func (h *harness) expectEvent(want string) {
	h.t.Helper()
	select {
	case got := <-h.events:
		require.Equal(h.t, want, got)
	case <-time.After(waitTimeout):
		h.t.Fatalf("no event %q", want)
	}
}

func (h *harness) expectNoEvent() {
	h.t.Helper()
	select {
	case got := <-h.events:
		h.t.Fatalf("unexpected event %q", got)
	default:
	}
}

func (h *harness) waitIdle() {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		st := h.stage.Status()
		return st.Running == nil && st.Pending == nil
	}, waitTimeout, 5*time.Millisecond, "stage did not become idle")
}

func (h *harness) waitLog(substrings ...string) logrus.Entry {
	h.t.Helper()
	var found logrus.Entry
	require.Eventually(h.t, func() bool {
		for _, e := range h.logs.AllEntries() {
			ok := true
			for _, s := range substrings {
				ok = ok && strings.Contains(e.Message, s)
			}
			if ok {
				found = *e
				return true
			}
		}
		return false
	}, waitTimeout, 5*time.Millisecond, "no log line containing %q", substrings)
	return found
}

// timedOffer fails the test when Offer takes 100 ms or more (or never returns).
func (h *harness) timedOffer(g *pb.Generation, op string) {
	h.t.Helper()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.stage.Offer(g, op)
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		require.Less(h.t, d, 100*time.Millisecond, "Offer(%s) blocked", g.Uuid)
	case <-time.After(waitTimeout):
		h.t.Fatalf("Offer(%s) did not return", g.Uuid)
	}
}

func (h *harness) readFile(name string) string {
	b, err := os.ReadFile(h.path(name))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(h.t, err)
	return string(b)
}

func gen(uuid string) *pb.Generation {
	return &pb.Generation{Uuid: uuid, OutPath: "/nix/store/" + uuid}
}

func background(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestOfferNeverBlocksAndSupersedes(t *testing.T) {
	h := newHarness(t, blockingHook)
	h.fifos("g1", "g4")
	h.run(background(t))

	h.timedOffer(gen("g1"), "switch")
	h.waitUp("g1")
	h.timedOffer(gen("g2"), "switch")
	h.timedOffer(gen("g3"), "switch")
	h.timedOffer(gen("g4"), "test")

	st := h.stage.Status()
	require.NotNil(t, st.Running)
	require.NotNil(t, st.Pending)
	assert.Equal(t, "g1", st.Running.Generation.Uuid)
	assert.Equal(t, "g4", st.Pending.Generation.Uuid)

	h.expectNoEvent()
	h.release("g1", 0)
	h.waitUp("g4")
	assert.Empty(t, h.submittedList(), "the superseded generation must not be submitted")
	h.expectNoEvent()

	h.release("g4", 0)
	h.expectEvent("submit:g4:test")
	h.waitIdle()

	assert.Equal(t, []string{"g4"}, h.submittedList())
	assert.Empty(t, h.failedList())
	assert.Equal(t, "g1\ng4\n", h.readFile("runs"), "hooks run only for g1 and g4")
	assert.Empty(t, h.readFile("overlap"), "two hooks ran at once")
	assert.Empty(t, h.readFile("signals"), "a hook received a signal")
	assert.Equal(t, "done\n", h.readFile("done.g1"), "g1's hook did not run to completion")
}

func TestEnvironment(t *testing.T) {
	const envHook = `#!/bin/sh
for v in PREPULL_INHERITED COMIN_GIT_SHA COMIN_GIT_REF COMIN_GIT_MSG COMIN_HOSTNAME COMIN_GENERATION COMIN_OPERATION COMIN_OUT_PATH; do
  eval "printf '%s' \"\$$v\"" > "$HOOKDIR/env.$v"
done
`
	for _, tc := range []struct{ op, wantOp string }{{"", "switch"}, {"test", "test"}} {
		t.Run("operation="+tc.op, func(t *testing.T) {
			t.Setenv("PREPULL_INHERITED", "from-the-test-process")
			h := newHarness(t, envHook)
			h.run(background(t))
			h.stage.Offer(&pb.Generation{
				Uuid:               "uuid-1",
				SelectedCommitId:   "abc123",
				SelectedRemoteName: "origin",
				SelectedBranchName: "main",
				SelectedCommitMsg:  "\n\nfix: the thing\n\nlonger body\n\n",
				Hostname:           "r213",
				OutPath:            "/nix/store/0000-nixos-system-r213",
			}, tc.op)
			h.expectEvent("submit:uuid-1:" + tc.op)

			for name, want := range map[string]string{
				"PREPULL_INHERITED": "from-the-test-process",
				"COMIN_GIT_SHA":     "abc123",
				"COMIN_GIT_REF":     "origin/main",
				"COMIN_GIT_MSG":     "fix: the thing\n\nlonger body",
				"COMIN_HOSTNAME":    "r213",
				"COMIN_GENERATION":  "uuid-1",
				"COMIN_OPERATION":   tc.wantOp,
				"COMIN_OUT_PATH":    "/nix/store/0000-nixos-system-r213",
			} {
				assert.Equal(t, want, h.readFile("env."+name), name)
			}
		})
	}
}

func TestOutputIsStreamedWhileTheHookRuns(t *testing.T) {
	h := newHarness(t, `#!/bin/sh
echo "to stdout"
echo "to stderr" >&2
read x < "$HOOKDIR/release.g1"
`)
	h.fifos("g1")
	h.run(background(t))
	h.stage.Offer(gen("g1"), "switch")

	out := h.waitLog("to stdout")
	errLine := h.waitLog("to stderr")
	for _, e := range []logrus.Entry{out, errLine} {
		assert.Equal(t, logrus.InfoLevel, e.Level)
		assert.Contains(t, e.Message, "pre-deployment command")
		assert.Contains(t, e.Message, "g1")
	}
	assert.Contains(t, out.Message, "stdout")
	assert.Contains(t, errLine.Message, "stderr")
	h.expectNoEvent()

	h.release("g1", 0)
	h.expectEvent("submit:g1:switch")
}

func TestVeryLongLineNeitherStallsNorDropsLaterLines(t *testing.T) {
	h := newHarness(t, `#!/bin/sh
head -c 1048576 /dev/zero | tr '\0' x
echo
echo after
`)
	h.run(background(t))
	h.stage.Offer(gen("g1"), "switch")

	h.expectEvent("submit:g1:switch")
	h.waitLog("after")
	for _, e := range h.logs.AllEntries() {
		assert.LessOrEqual(t, len(e.Message), maxLogLine+256, "a logged line is not bounded")
	}
}

func TestFailure(t *testing.T) {
	t.Run("exit 1", func(t *testing.T) {
		h := newHarness(t, blockingHook)
		h.fifos("g1")
		h.run(background(t))
		h.stage.Offer(gen("g1"), "test")
		h.waitUp("g1")
		h.release("g1", 1)
		h.expectEvent("fail:g1:test")
		h.assertFailedOnce("g1", "test", "exit status 1")
	})
	t.Run("killed by a signal", func(t *testing.T) {
		h := newHarness(t, blockingHook)
		h.fifos("g1")
		h.run(background(t))
		h.stage.Offer(gen("g1"), "test")
		h.waitUp("g1")
		pid, err := strconv.Atoi(strings.TrimSpace(h.readFile("pid.g1")))
		require.NoError(t, err)
		require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
		h.expectEvent("fail:g1:test")
		h.assertFailedOnce("g1", "test", "killed")
	})
	t.Run("command does not exist", func(t *testing.T) {
		h := newHarness(t, blockingHook)
		h.stage = New(h.path("does-not-exist"), h.onSubmit, h.onFail)
		h.run(background(t))
		h.stage.Offer(gen("g1"), "test")
		h.expectEvent("fail:g1:test")
		h.assertFailedOnce("g1", "test", "")
	})
}

func (h *harness) assertFailedOnce(uuid, op, errContains string) {
	h.t.Helper()
	h.waitIdle()
	assert.Empty(h.t, h.submittedList())
	failed := h.failedList()
	require.Len(h.t, failed, 1)
	assert.Equal(h.t, uuid, failed[0].g.Uuid)
	assert.Equal(h.t, op, failed[0].op)
	require.Error(h.t, failed[0].err)
	assert.Contains(h.t, failed[0].err.Error(), errContains)
	h.expectNoEvent()

	entry := h.waitLog("failed", uuid)
	assert.Equal(h.t, logrus.ErrorLevel, entry.Level)
}

func TestFailureWithPendingStartsPendingAndDoesNotCallFail(t *testing.T) {
	h := newHarness(t, blockingHook)
	h.fifos("g1", "g2")
	h.run(background(t))

	h.stage.Offer(gen("g1"), "switch")
	h.waitUp("g1")
	h.stage.Offer(gen("g2"), "switch")
	h.release("g1", 1)
	h.waitUp("g2")

	assert.Empty(t, h.failedList(), "fail was called for a superseded generation")
	assert.Empty(t, h.submittedList())
	entry := h.waitLog("failed", "g1")
	assert.Equal(t, logrus.ErrorLevel, entry.Level)

	h.release("g2", 0)
	h.expectEvent("submit:g2:switch")
	h.waitIdle()
	assert.Empty(t, h.failedList())
	assert.Equal(t, []string{"g2"}, h.submittedList())
}

func TestStatus(t *testing.T) {
	h := newHarness(t, blockingHook)
	h.fifos("g1", "g2")
	h.run(background(t))

	st := h.stage.Status()
	assert.Nil(t, st.Running)
	assert.Nil(t, st.Pending)

	before := time.Now()
	g1, g2 := gen("g1"), gen("g2")
	h.stage.Offer(g1, "test")
	h.waitUp("g1")
	h.stage.Offer(g2, "switch")

	st = h.stage.Status()
	require.NotNil(t, st.Running)
	assert.Same(t, g1, st.Running.Generation)
	assert.Equal(t, "test", st.Running.Operation)
	assert.False(t, st.Running.StartedAt.IsZero())
	assert.False(t, st.Running.StartedAt.Before(before))
	assert.False(t, st.Running.StartedAt.After(time.Now()))
	require.NotNil(t, st.Pending)
	assert.Same(t, g2, st.Pending.Generation)
	assert.Equal(t, "switch", st.Pending.Operation)
	assert.True(t, st.Pending.StartedAt.IsZero())

	st.Running.Operation = "mutated"
	assert.Equal(t, "test", h.stage.Status().Running.Operation, "Status is not a copy")

	h.release("g1", 0)
	h.waitUp("g2")
	st = h.stage.Status()
	require.NotNil(t, st.Running)
	assert.Same(t, g2, st.Running.Generation)
	assert.False(t, st.Running.StartedAt.IsZero())
	assert.Nil(t, st.Pending)

	h.release("g2", 0)
	h.expectEvent("submit:g2:switch")
	h.waitIdle()
}

func TestContextCancelStopsTheStage(t *testing.T) {
	h := newHarness(t, blockingHook)
	h.fifos("g1", "g2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.run(ctx)

	h.stage.Offer(gen("g1"), "switch")
	h.waitUp("g1")
	h.stage.Offer(gen("g2"), "switch")
	cancel()
	h.waitIdle()
	h.stage.Offer(gen("g3"), "switch")

	st := h.stage.Status()
	assert.Nil(t, st.Running, "Offer after cancel started something")
	assert.Nil(t, st.Pending)
	h.expectNoEvent()
	assert.Empty(t, h.submittedList())
	assert.Empty(t, h.failedList())
	assert.Equal(t, "g1\n", h.readFile("runs"), "a hook started after cancel")
}

func TestBackgroundChildHoldingOutputDoesNotBlockSuccess(t *testing.T) {
	old := waitDelay
	waitDelay = 100 * time.Millisecond
	t.Cleanup(func() { waitDelay = old })
	h := newHarness(t, "#!/bin/sh\nsleep 3 &\nexit 0\n")
	h.run(background(t))

	start := time.Now()
	h.stage.Offer(gen("g1"), "switch")
	h.expectEvent("submit:g1:switch")
	assert.Less(t, time.Since(start), 2*time.Second, "Wait was held by the background child")
}
