package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	waitDelay     = 5 * time.Second
	maxReasonSize = 4096
)

// Command is an externally configured admission check. Path "" means none is configured.
type Command struct {
	Path    string
	Timeout time.Duration
}

// Verdict is the outcome of Ask. Reason is the child's first stdout line; when that
// line is empty, a refusal gets a "preflight: " line naming the cause. The line is
// cut to 4096 bytes before invalid UTF-8 becomes U+FFFD, so Reason can exceed 4096 bytes.
type Verdict struct {
	Admitted bool
	Reason   string
}

// Ask runs c with os.Environ() followed by env. It admits iff c starts and exits 0
// before c.Timeout and before ctx ends; Path "" admits without starting a process,
// and Timeout <= 0 with a Path panics. It kills the child's whole process group
// before returning, and returns within c.Timeout + 5s even when a grandchild holds stdout.
func Ask(ctx context.Context, c Command, env []string) Verdict {
	if c.Path == "" {
		return Verdict{Admitted: true}
	}
	if c.Timeout <= 0 {
		panic("preflight: Command.Timeout must be positive")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stdout := &firstLine{}
	stderr := logrus.WithField("preflight", c.Path).WriterLevel(logrus.InfoLevel)
	defer func() { _ = stderr.Close() }()

	var endedByCtx atomic.Bool // set only when runCtx ended while the leader still ran
	cmd := exec.CommandContext(runCtx, c.Path)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		endedByCtx.Store(true)
		return killGroup(cmd.Process.Pid)
	}

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return refuse("", "context ended: "+ctx.Err().Error())
		}
		return refuse("", "cannot start: "+err.Error())
	}

	// Only a leader still running at the deadline times out: signal 0 reports
	// os.ErrProcessDone once Wait has reaped an earlier exit.
	var timedOut atomic.Bool
	timer := time.AfterFunc(c.Timeout, func() {
		if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return
		}
		timedOut.Store(true)
		cancel()
	})
	err := cmd.Wait()
	timer.Stop()

	// A leftover group member (a grandchild holding stdout) dies before the verdict is read.
	_ = killGroup(cmd.Process.Pid)

	line := stdout.line()
	switch {
	case timedOut.Load():
		return refuse(line, fmt.Sprintf("timed out after %s", c.Timeout))
	case err == nil:
		return Verdict{Admitted: true, Reason: line}
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success():
		// The leader exited 0 on time; only its stdout pipe was still held.
		return Verdict{Admitted: true, Reason: line}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			if endedByCtx.Load() && ctx.Err() != nil {
				return refuse(line, "context ended: "+ctx.Err().Error())
			}
			return refuse(line, fmt.Sprintf("killed by signal %d (%s)", ws.Signal(), ws.Signal()))
		}
		return refuse(line, fmt.Sprintf("exit status %d", exitErr.ExitCode()))
	}
	if ctx.Err() != nil {
		return refuse(line, "context ended: "+ctx.Err().Error())
	}
	return refuse(line, "wait failed: "+err.Error())
}

// refuse prefers the child's own line; the comin-authored cause is logged either way.
func refuse(line, cause string) Verdict {
	logrus.Warnf("preflight refused: %s", cause)
	if line != "" {
		return Verdict{Reason: line}
	}
	return Verdict{Reason: "preflight: " + cause}
}

// killGroup SIGKILLs pid's process group; a group that is already gone is fine.
func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// firstLine keeps the first line written to it (capped) and discards the rest, so the
// child's stdout is always drained.
type firstLine struct {
	buf  []byte
	done bool
}

func (f *firstLine) Write(p []byte) (int, error) {
	if f.done {
		return len(p), nil
	}
	chunk := p
	if i := bytes.IndexByte(p, '\n'); i >= 0 {
		chunk = p[:i]
		f.done = true
	}
	if room := maxReasonSize - len(f.buf); len(chunk) > room {
		chunk = chunk[:room]
	}
	f.buf = append(f.buf, chunk...)
	return len(p), nil
}

func (f *firstLine) line() string {
	return strings.ToValidUTF8(string(bytes.TrimRight(f.buf, "\r")), "\uFFFD")
}
