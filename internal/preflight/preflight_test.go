package preflight_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nlewo/comin/internal/preflight"
	"github.com/nlewo/comin/internal/protobuf"
	"google.golang.org/protobuf/encoding/protojson"
)

func script(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "preflight.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAskAdmitsIffExitZeroBeforeTimeout(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		path    string
		timeout time.Duration
		cancel  time.Duration
		admit   bool
		reason  string
	}{
		{"exit 0", script(t, "exit 0\n", 0o755), 10 * time.Second, 0, true, ""},
		{"exit 1", script(t, "exit 1\n", 0o755), 10 * time.Second, 0, false, "preflight: exit status 1"},
		{"exit 0 after timeout", script(t, "sleep 5\nexit 0\n", 0o755), 300 * time.Millisecond, 0, false, "preflight: timed out after 300ms"},
		{"killed by signal", script(t, "kill -9 $$\n", 0o755), 10 * time.Second, 0, false, "preflight: killed by signal 9 "},
		{"not executable", script(t, "exit 0\n", 0o644), 10 * time.Second, 0, false, "preflight: cannot start: "},
		{"path missing", filepath.Join(dir, "missing"), 10 * time.Second, 0, false, "preflight: cannot start: "},
		{"ctx cancelled while running", script(t, "sleep 30\nexit 0\n", 0o755), 30 * time.Second, 200 * time.Millisecond, false, "preflight: context ended: context canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel > 0 {
				time.AfterFunc(tc.cancel, cancel)
			}
			start := time.Now()
			v := preflight.Ask(ctx, preflight.Command{Path: tc.path, Timeout: tc.timeout}, nil)
			if v.Admitted != tc.admit {
				t.Fatalf("Admitted = %v, want %v (reason %q)", v.Admitted, tc.admit, v.Reason)
			}
			if !strings.HasPrefix(v.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want prefix %q", v.Reason, tc.reason)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("Ask took %s", d)
			}
		})
	}
}

// alive reports whether pid exists in /proc and is not a zombie.
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	return s[strings.LastIndexByte(s, ')')+2] != 'Z'
}

func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// askBounded fails the test unless Ask returns within bound.
func askBounded(t *testing.T, ctx context.Context, path string, timeout, bound time.Duration) preflight.Verdict {
	t.Helper()
	start := time.Now()
	done := make(chan preflight.Verdict, 1)
	go func() {
		done <- preflight.Ask(ctx, preflight.Command{Path: path, Timeout: timeout}, nil)
	}()
	select {
	case v := <-done:
		if d := time.Since(start); d > bound {
			t.Fatalf("Ask took %s, want <= %s", d, bound)
		}
		return v
	case <-time.After(bound + 2*time.Second):
		t.Fatalf("Ask did not return within %s", bound)
	}
	panic("unreachable")
}

// guardPID best-effort kills the pid recorded in pidFile, so a grandchild no
// code path kills never outlives the run.
func guardPID(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 || !alive(pid) {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
}

// waitDead polls up to 2s for pid to leave the live set.
func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("process %d still alive after Ask returned", pid)
	}
}

// askWithGrandchild runs a script that starts sleep 1000 holding stdout, records
// its pid, then runs body.
func askWithGrandchild(t *testing.T, body string, timeout, cancelAfter time.Duration) (preflight.Verdict, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := script(t, "sleep 1000 &\necho $! > "+pidFile+"\n"+body, 0o755)
	guardPID(t, pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, cancel)
	}
	return askBounded(t, ctx, p, timeout, timeout+5*time.Second), pidFile
}

func TestAskKillsGroupWhateverTheVerdict(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	cases := []struct {
		name     string
		body     string
		timeout  time.Duration
		cancel   time.Duration
		admitted bool
		reason   string
	}{
		{"admit", "exit 0\n", 2 * time.Second, 0, true, ""},
		{"timeout", "wait\n", time.Second, 0, false, "preflight: timed out after 1s"},
		{"ctx end", "wait\n", 30 * time.Second, time.Second, false, "preflight: context ended: context canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, pidFile := askWithGrandchild(t, tc.body, tc.timeout, tc.cancel)
			if v.Admitted != tc.admitted {
				t.Fatalf("Admitted = %v, want %v (reason %q)", v.Admitted, tc.admitted, v.Reason)
			}
			if !strings.HasPrefix(v.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want prefix %q", v.Reason, tc.reason)
			}
			waitDead(t, readPID(t, pidFile))
		})
	}
}

// The leader's exit cause survives a later timer or ctx end while a grandchild holds stdout.
func TestAskNamesLeadersExitCauseWhenGrandchildHoldsStdout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	cases := []struct {
		name    string
		body    string
		timeout time.Duration
		cancel  time.Duration
		reason  string
	}{
		{"exit status before the timer", "exit 1\n", time.Second, 0, "preflight: exit status 1"},
		{"exit status before ctx end", "exit 1\n", 30 * time.Second, time.Second, "preflight: exit status 1"},
		{"signal before the timer", "kill -9 $$\n", time.Second, 0, "preflight: killed by signal 9 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, pidFile := askWithGrandchild(t, tc.body, tc.timeout, tc.cancel)
			if v.Admitted {
				t.Fatalf("admitted: %+v", v)
			}
			if !strings.HasPrefix(v.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want prefix %q", v.Reason, tc.reason)
			}
			waitDead(t, readPID(t, pidFile))
		})
	}
}

// Ask returns within Timeout+5s when a grandchild, in or out of the group, holds stdout.
func TestAskReturnsBoundedWhenGrandchildHoldsStdout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	t.Run("grandchild in the group", func(t *testing.T) {
		v, pidFile := askWithGrandchild(t, "exit 0\n", 2*time.Second, 0)
		if !v.Admitted {
			t.Fatalf("not admitted: %+v", v)
		}
		waitDead(t, readPID(t, pidFile))
	})
	t.Run("grandchild left the group", func(t *testing.T) {
		if _, err := exec.LookPath("setsid"); err != nil {
			t.Skip("setsid not available")
		}
		pidFile := filepath.Join(t.TempDir(), "pid")
		p := script(t, "setsid sh -c 'echo $$ > "+pidFile+"; exec sleep 1000' &\nexit 0\n", 0o755)
		guardPID(t, pidFile)
		v := askBounded(t, context.Background(), p, 2*time.Second, 2*time.Second+5*time.Second)
		if !v.Admitted {
			t.Fatalf("not admitted: %+v", v)
		}
	})
}

func TestAskReasonIsFirstStdoutLine(t *testing.T) {
	t.Run("first line of a refusal", func(t *testing.T) {
		p := script(t, "printf 'refused: gate-a\\nsecond\\n'\nexit 3\n", 0o755)
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		if v.Admitted || v.Reason != "refused: gate-a" {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("carriage return trimmed", func(t *testing.T) {
		p := script(t, "printf 'no\\r\\n'\nexit 1\n", 0o755)
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		if v.Admitted || v.Reason != "no" {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("empty first line falls back to cause", func(t *testing.T) {
		p := script(t, "printf '\\nlater\\n'\nexit 2\n", 0o755)
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		if v.Admitted || v.Reason != "preflight: exit status 2" {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("1 MiB of stdout does not stall", func(t *testing.T) {
		p := script(t, "echo first\nhead -c 1048576 /dev/zero | tr '\\0' 'x'\nexit 0\n", 0o755)
		start := time.Now()
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		if !v.Admitted || v.Reason != "first" {
			t.Fatalf("got %+v", v)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("Ask took %s", d)
		}
	})
	t.Run("cap truncates at 4096 bytes", func(t *testing.T) {
		data := filepath.Join(t.TempDir(), "line")
		if err := os.WriteFile(data, []byte(strings.Repeat("€", 1500)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p := script(t, "cat "+data+"\nexit 3\n", 0o755)
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		want := strings.Repeat("€", 1365) + "\uFFFD"
		if v.Admitted || v.Reason != want {
			t.Fatalf("got admitted=%v len=%d, want len=%d", v.Admitted, len(v.Reason), len(want))
		}
	})
}

// A first line that is not valid UTF-8 still yields a sanitized Reason.
func TestAskReasonIsValidUTF8(t *testing.T) {
	run := func(body string) preflight.Verdict {
		p := script(t, body, 0o755)
		return preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
	}
	invalidLine := func(t *testing.T) string {
		t.Helper()
		data := filepath.Join(t.TempDir(), "line")
		if err := os.WriteFile(data, append(bytes.Repeat([]byte{0xFF}, 5000), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		v := run("cat " + data + "\nexit 3\n")
		if v.Admitted {
			t.Fatalf("admitted: %+v", v)
		}
		return v.Reason
	}

	t.Run("invalid bytes still yield the child's line", func(t *testing.T) {
		reason := invalidLine(t)
		if reason == "" || !utf8.ValidString(reason) {
			t.Fatalf("Reason = %q, want non-empty valid UTF-8", reason)
		}
		if strings.HasPrefix(reason, "preflight: ") {
			t.Fatalf("Reason replaced by comin line: %q", reason)
		}
		if strings.Trim(reason, "\uFFFD") != "" {
			t.Fatalf("Reason = %q, want only replacement runes from the 0xFF line", reason)
		}
	})

	t.Run("rune split across writes", func(t *testing.T) {
		p := script(t, "head -c 4095 /dev/zero | tr '\\0' x\nprintf '\\342'\nsleep 0.2\nprintf '\\202\\254 tail\\n'\nexit 3\n", 0o755)
		v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, nil)
		if v.Admitted {
			t.Fatalf("admitted: %+v", v)
		}
		if !utf8.ValidString(v.Reason) {
			t.Fatalf("Reason is not valid UTF-8: %q", v.Reason)
		}
		if strings.TrimRight(v.Reason, "\uFFFD") != strings.Repeat("x", 4095) {
			t.Fatalf("Reason = %q, want 4095 x's then a replacement rune", v.Reason)
		}
	})

	t.Run("protojson round-trip", func(t *testing.T) {
		reason := invalidLine(t)
		if _, err := protojson.Marshal(&protobuf.Deployment{ErrorMsg: reason}); err != nil {
			t.Fatalf("protojson.Marshal: %v", err)
		}
	})
}

func envSet(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for _, e := range strings.Split(string(b), "\x00") {
		if e != "" {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func TestAskEnvironmentAppendsToOwn(t *testing.T) {
	t.Setenv("PREFLIGHT_TEST_INHERITED", "yes")
	out := filepath.Join(t.TempDir(), "env")
	p := script(t, "env -0 > "+out+"\n", 0o755)
	extra := []string{"COMIN_TEST_A=1", "COMIN_TEST_B=two words"}

	v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, extra)
	if !v.Admitted {
		t.Fatalf("got %+v", v)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := envSet(t, b)

	// The shell may add variables of its own; a reference run with no extras absorbs them.
	refOut := filepath.Join(t.TempDir(), "ref")
	ref := script(t, "env -0 > "+refOut+"\n", 0o755)
	if v := preflight.Ask(context.Background(), preflight.Command{Path: ref, Timeout: 10 * time.Second}, nil); !v.Admitted {
		t.Fatalf("reference: %+v", v)
	}
	rb, err := os.ReadFile(refOut)
	if err != nil {
		t.Fatal(err)
	}
	want := append(envSet(t, rb), extra...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestAskLaterEnvEntryWins(t *testing.T) {
	t.Setenv("PREFLIGHT_TEST_DUP", "old")
	out := filepath.Join(t.TempDir(), "dup")
	p := script(t, "printf %s \"$PREFLIGHT_TEST_DUP\" > "+out+"\n", 0o755)
	if v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: 10 * time.Second}, []string{"PREFLIGHT_TEST_DUP=new"}); !v.Admitted {
		t.Fatalf("got %+v", v)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("got %q, want new", b)
	}
}

func TestAskEmptyPathAdmitsWithoutProcess(t *testing.T) {
	v := preflight.Ask(context.Background(), preflight.Command{}, []string{"A=1"})
	if !v.Admitted || v.Reason != "" {
		t.Fatalf("got %+v", v)
	}
}
