package preflight_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/preflight"
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

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	return s[strings.LastIndexByte(s, ')')+2] != 'Z'
}

func TestAskTimeoutKillsWholeProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := script(t, "sleep 1000 &\necho $! > "+pidFile+"\nwait\n", 0o755)

	timeout := time.Second
	start := time.Now()
	v := preflight.Ask(context.Background(), preflight.Command{Path: p, Timeout: timeout}, nil)
	elapsed := time.Since(start)

	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if alive(pid) {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	})

	if v.Admitted {
		t.Fatalf("admitted on timeout: %+v", v)
	}
	if elapsed > timeout+5*time.Second {
		t.Fatalf("Ask took %s, want <= %s", elapsed, timeout+5*time.Second)
	}
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("grandchild %d still alive after Ask returned", pid)
	}
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
