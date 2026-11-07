package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nlewo/comin/internal/fetcher"
	"github.com/nlewo/comin/internal/syncgate"
	"github.com/stretchr/testify/require"
)

// The gate script of the rig logs every run to gate.log as
// stage|trigger|answer|start|end (UTC nanoseconds) and is closed while a
// file gate-closed[-<key>] exists, key being the stage or the trigger of
// the run. A closed run prints reason gateReason unless the file
// gate-silent exists. A run sleeps the seconds in the file gate-sleep-<stage>.

const gateReason = "closed by test"

type gateRun struct {
	stage, trigger, answer string
	start, end             time.Time
}

func (g gateRun) duration() time.Duration { return g.end.Sub(g.start) }

func (r *rig) gateCommand() syncgate.Command {
	if !r.opts.gate {
		return syncgate.Command{}
	}
	path := r.opts.gatePath
	if path == "" {
		path = filepath.Join(r.scriptDir, "gate.sh")
		body := fmt.Sprintf(`#!/bin/sh
d=%q
t0=$(date +%%s%%N)
mode=open
for k in "" "-$COMIN_TRIGGER" "-$COMIN_SYNC_STAGE"; do
  [ -e "$d/gate-closed$k" ] && mode=closed
done
if [ -e "$d/gate-sleep-$COMIN_SYNC_STAGE" ]; then read secs < "$d/gate-sleep-$COMIN_SYNC_STAGE"; sleep "$secs"; fi
t1=$(date +%%s%%N)
printf '%%s|%%s|%%s|%%s|%%s\n' "$COMIN_SYNC_STAGE" "$COMIN_TRIGGER" "$mode" "$t0" "$t1" >> "$d/gate.log"
if [ "$mode" = closed ]; then
  [ -e "$d/gate-silent" ] || echo %q
  exit 1
fi
`, r.scriptDir, gateReason)
		require.NoError(r.t, os.WriteFile(path, []byte(body), 0o755))
	}
	timeout := r.opts.gateTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return syncgate.Command{Path: path, Timeout: timeout}
}

func (r *rig) fetcherOpts() []fetcher.Option {
	if !r.opts.gate {
		return nil
	}
	return []fetcher.Option{fetcher.WithSyncGate(r.gateCommand())}
}

func (r *rig) managerOpts() []Option {
	if !r.opts.gate {
		return nil
	}
	return []Option{WithSyncGate(r.gateCommand())}
}

// gateSet closes or opens the gate for key: "" for every run, else a
// stage or a trigger.
func (r *rig) gateSet(key string, closed bool) {
	r.t.Helper()
	name := "gate-closed"
	if key != "" {
		name += "-" + key
	}
	p := filepath.Join(r.scriptDir, name)
	if closed {
		require.NoError(r.t, os.WriteFile(p, nil, 0o644))
		return
	}
	_ = os.Remove(p)
}

func (r *rig) gateFile(name string, on bool, content string) {
	r.t.Helper()
	p := filepath.Join(r.scriptDir, name)
	if on {
		require.NoError(r.t, os.WriteFile(p, []byte(content), 0o644))
		return
	}
	_ = os.Remove(p)
}

func (r *rig) gateRuns() []gateRun {
	b, _ := os.ReadFile(filepath.Join(r.scriptDir, "gate.log"))
	var out []gateRun
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 {
			continue
		}
		start, err1 := strconv.ParseInt(f[3], 10, 64)
		end, err2 := strconv.ParseInt(f[4], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, gateRun{f[0], f[1], f[2], time.Unix(0, start), time.Unix(0, end)})
	}
	return out
}

// gatePairs counts the runs by "<stage> <trigger>".
func (r *rig) gatePairs() map[string]int {
	out := map[string]int{}
	for _, g := range r.gateRuns() {
		out[g.stage+" "+g.trigger]++
	}
	return out
}

// gateRunsOf returns the runs of stage.
func (r *rig) gateRunsOf(stage string) []gateRun {
	var out []gateRun
	for _, g := range r.gateRuns() {
		if g.stage == stage {
			out = append(out, g)
		}
	}
	return out
}

// firstClosed returns the first closed run logged.
func (r *rig) firstClosed() gateRun {
	for _, g := range r.gateRuns() {
		if g.answer == "closed" {
			return g
		}
	}
	return gateRun{}
}
