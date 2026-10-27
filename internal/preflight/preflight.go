package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// Verdict is the outcome of Ask. Reason is the first stdout line of the child, or a
// line starting "preflight: " naming the cause of a refusal that printed nothing.
type Verdict struct {
	Admitted bool
	Reason   string
}

// Ask runs c with os.Environ() followed by env and admits iff it starts and exits 0
// before c.Timeout (measured from start) and before ctx ends. Every other outcome
// refuses; a Command with Path "" admits without starting a process. On timeout or ctx
// end the child's whole process group is killed and Ask returns within c.Timeout + 5s.
// c.Timeout <= 0 with a non-empty Path is a caller bug and panics.
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

	cmd := exec.CommandContext(runCtx, c.Path)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return refuse("", "context ended: "+ctx.Err().Error())
		}
		return refuse("", "cannot start: "+err.Error())
	}

	var timedOut atomic.Bool
	timer := time.AfterFunc(c.Timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	err := cmd.Wait()
	timer.Stop()

	line := stdout.line()
	switch {
	case err == nil:
		return Verdict{Admitted: true, Reason: line}
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success():
		return Verdict{Admitted: true, Reason: line}
	case timedOut.Load():
		return refuse(line, fmt.Sprintf("timed out after %s", c.Timeout))
	case ctx.Err() != nil:
		return refuse(line, "context ended: "+ctx.Err().Error())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return refuse(line, fmt.Sprintf("killed by signal %d (%s)", ws.Signal(), ws.Signal()))
		}
		return refuse(line, fmt.Sprintf("exit status %d", exitErr.ExitCode()))
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
	return string(bytes.TrimRight(f.buf, "\r"))
}
