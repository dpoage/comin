package syncgate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gate.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755))
	return p
}

func TestWatchPeriodIsAtMostTwoSeconds(t *testing.T) {
	assert.LessOrEqual(t, WatchPeriod, 2*time.Second)
	assert.Positive(t, WatchPeriod)
}

func TestAsk(t *testing.T) {
	tests := []struct {
		name    string
		cmd     func(t *testing.T) Command
		open    bool
		reason  string
		contain string
	}{
		{name: "no command is open", cmd: func(*testing.T) Command { return Command{} }, open: true},
		{name: "exit 0 is open", cmd: func(t *testing.T) Command {
			return Command{Path: script(t, "exit 0\n"), Timeout: 5 * time.Second}
		}, open: true},
		{name: "exit 0 with a line is open", cmd: func(t *testing.T) Command {
			return Command{Path: script(t, "echo window open\nexit 0\n"), Timeout: 5 * time.Second}
		}, open: true},
		{name: "non-zero exit is closed with the first line", cmd: func(t *testing.T) Command {
			return Command{Path: script(t, "echo 'update-window: opens at 23:00'\necho second\nexit 1\n"), Timeout: 5 * time.Second}
		}, reason: "update-window: opens at 23:00"},
		{name: "non-zero exit without a line names the failure", cmd: func(t *testing.T) Command {
			return Command{Path: script(t, "exit 7\n"), Timeout: 5 * time.Second}
		}, contain: "exit status 7"},
		{name: "timeout is closed", cmd: func(t *testing.T) Command {
			return Command{Path: script(t, "sleep 30\n"), Timeout: 200 * time.Millisecond}
		}, contain: "timed out"},
		{name: "missing binary is closed", cmd: func(t *testing.T) Command {
			return Command{Path: filepath.Join(t.TempDir(), "absent"), Timeout: 5 * time.Second}
		}, contain: "cannot start"},
		{name: "not executable is closed", cmd: func(t *testing.T) Command {
			p := filepath.Join(t.TempDir(), "gate")
			require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o644))
			return Command{Path: p, Timeout: 5 * time.Second}
		}, contain: "cannot start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.cmd(t).Ask(t.Context(), StagePass, Poll)
			assert.Equal(t, tt.open, a.Open)
			if tt.open {
				assert.Empty(t, a.Reason)
				return
			}
			if tt.reason != "" {
				assert.Equal(t, tt.reason, a.Reason)
			}
			assert.Contains(t, a.Reason, tt.contain)
			assert.NotEmpty(t, a.Reason)
		})
	}
}

func TestAskEnvironment(t *testing.T) {
	t.Setenv("COMIN_TRIGGER", "inherited")
	t.Setenv("COMIN_SYNC_STAGE", "inherited")
	t.Setenv("FROM_COMIN", "kept")
	out := filepath.Join(t.TempDir(), "env")
	c := Command{Path: script(t, `echo "$COMIN_SYNC_STAGE $COMIN_TRIGGER $FROM_COMIN" > `+out+"\n"), Timeout: 5 * time.Second}
	for _, stage := range []Stage{StageFetch, StagePass, StageWatch} {
		for _, trigger := range []Trigger{Poll, Manual} {
			require.True(t, c.Ask(context.Background(), stage, trigger).Open)
			b, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Equal(t, string(stage)+" "+string(trigger)+" kept\n", string(b))
		}
	}
}

func TestAskKillsTheProcessGroupAtTheTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survivor")
	c := Command{Path: script(t, "(sleep 2; touch "+marker+") &\nsleep 30\n"), Timeout: 200 * time.Millisecond}
	start := time.Now()
	a := c.Ask(t.Context(), StageWatch, Poll)
	assert.False(t, a.Open)
	assert.Less(t, time.Since(start), 5*time.Second)
	time.Sleep(2500 * time.Millisecond)
	assert.NoFileExists(t, marker, "a grandchild outlived the gate run")
}
