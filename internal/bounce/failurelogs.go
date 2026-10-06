package bounce

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
)

// DefaultFailureLogLines is how many trailing lines FailureLogs keeps when
// Lines is not set.
const DefaultFailureLogLines = 200

// failureLogsTimeout bounds reading a replica's logs: the replica is about to
// be removed, and a stuck daemon must not hold the failure up.
const failureLogsTimeout = 10 * time.Second

// FailureLogs is where a new replica that is removed for never turning healthy
// leaves its last log lines first. Those lines are the only record of why it
// did not start, and they go with the container.
type FailureLogs struct {
	Dir   string // empty: nothing is saved
	Lines int    // trailing lines to keep; 0 means DefaultFailureLogLines
}

func (f FailureLogs) lines() int {
	if f.Lines > 0 {
		return f.Lines
	}
	return DefaultFailureLogLines
}

// saveFailureLogs writes o's last lines to Dir/<replica>-<UTC timestamp>.log,
// through a temporary file so a reader never sees a partial one. It is best
// effort: a failure is reported on the replica's row and changes nothing else,
// the bounce's result included.
func (r *Runner) saveFailureLogs(ctx context.Context, o Observed) {
	if r.FailureLogs.Dir == "" {
		return
	}
	path, err := r.writeFailureLogs(ctx, o)
	if err != nil {
		r.event(container(o.Name), api.Warning, "Logs not saved:", err.Error())
		return
	}
	r.event(container(o.Name), api.Done, "Logs saved", path)
}

func (r *Runner) writeFailureLogs(ctx context.Context, o Observed) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureLogsTimeout)
	defer cancel()
	out, err := r.Engine.Logs(ctx, o.ID, r.FailureLogs.lines())
	if err != nil {
		return "", err
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := os.MkdirAll(r.FailureLogs.Dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(r.FailureLogs.Dir, fmt.Sprintf("%s-%s.log", o.Name, r.Now().UTC().Format("20060102T150405Z")))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}
