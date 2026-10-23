package predeploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	pb "github.com/nlewo/comin/internal/protobuf"

	"github.com/sirupsen/logrus"
)

// A background child holding the output pipes must not keep Wait from returning.
var waitDelay = 10 * time.Second

// Longer lines are split, not dropped.
const maxLogLine = 64 << 10

// hookEnv mirrors the variable definitions of internal/deployer/post_deployment_command.go.
func hookEnv(g *pb.Generation, operation string) []string {
	if operation == "" {
		operation = "switch"
	}
	return append(os.Environ(),
		"COMIN_GIT_SHA="+g.SelectedCommitId,
		"COMIN_GIT_REF="+fmt.Sprintf("%s/%s", g.SelectedRemoteName, g.SelectedBranchName),
		"COMIN_GIT_MSG="+strings.Trim(g.SelectedCommitMsg, "\n"),
		"COMIN_HOSTNAME="+g.Hostname,
		"COMIN_GENERATION="+g.Uuid,
		"COMIN_OPERATION="+operation,
		"COMIN_OUT_PATH="+g.OutPath,
	)
}

// runHook returns non-nil when the command failed to start, exited non-zero or was killed by a signal.
func runHook(ctx context.Context, command string, g *pb.Generation, operation string) error {
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = hookEnv(g, operation)
	cmd.WaitDelay = waitDelay
	stdout := &lineLogger{generation: g.Uuid, stream: "stdout"}
	stderr := &lineLogger{generation: g.Uuid, stream: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	stdout.flush()
	stderr.flush()

	// The command succeeded; only a leftover child kept the pipes open.
	if errors.Is(err, exec.ErrWaitDelay) {
		logrus.Warnf("pre-deployment command for generation %s exited 0 but a child kept its output open for more than %s", g.Uuid, waitDelay)
		return nil
	}
	return err
}

// lineLogger logs each line at Info level as it arrives. Write never fails
// and splits lines longer than maxLogLine; logrus is synchronous, so a slow
// log sink stalls the writing process.
type lineLogger struct {
	generation string
	stream     string
	partial    []byte
}

func (w *lineLogger) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk, rest, complete := bytes.Cut(p, []byte{'\n'})
		w.partial = append(w.partial, chunk...)
		for len(w.partial) > maxLogLine {
			w.emit(w.partial[:maxLogLine])
			w.partial = append(w.partial[:0], w.partial[maxLogLine:]...)
		}
		if complete {
			w.emit(w.partial)
			w.partial = w.partial[:0]
		}
		p = rest
	}
	return n, nil
}

func (w *lineLogger) flush() {
	if len(w.partial) > 0 {
		w.emit(w.partial)
		w.partial = nil
	}
}

func (w *lineLogger) emit(line []byte) {
	logrus.Infof("pre-deployment command [generation %s, %s]: %s", w.generation, w.stream, line)
}
