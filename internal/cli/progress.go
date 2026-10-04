package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
)

// newEvents picks the display for --progress, with Compose's mode names and
// renderers (cmd/display); only json and quiet are Bouncer's own.
func newEvents(dockerCli command.Cli, mode string, timestamps bool) (api.EventProcessor, error) {
	if os.Getenv("NO_COLOR") != "" {
		display.NoColor()
	}
	out := io.Writer(dockerCli.Err())
	if timestamps {
		out = stamped{out, time.Now}
	}
	switch mode {
	case display.ModeAuto, "":
		if dockerCli.Err().IsTerminal() {
			return display.Full(dockerCli.Err(), dockerCli.Err(), true), nil
		}
		return display.Plain(out), nil
	case display.ModeTTY:
		return display.Full(dockerCli.Err(), dockerCli.Err(), true), nil
	case display.ModePlain:
		return display.Plain(out), nil
	case display.ModeJSON:
		return &jsonEvents{out: dockerCli.Err(), now: time.Now}, nil
	case display.ModeQuiet:
		return errorsOnly{display.Plain(out)}, nil
	}
	return nil, configErr(`--progress: %q is not "auto", "tty", "plain", "json" or "quiet"`, mode)
}

// inner is the display as Compose sees it: the command brackets the whole run
// with Start/Done, so Compose's operations and Bouncer's own events share one
// block instead of each Compose call opening and closing its own.
type inner struct{ api.EventProcessor }

func (inner) Start(context.Context, string) {}
func (inner) Done(string, bool)             {}

// errorsOnly is --progress quiet: Compose's quiet drops errors too.
type errorsOnly struct{ api.EventProcessor }

func (q errorsOnly) On(events ...api.Resource) {
	for _, e := range events {
		if e.Status == api.Error {
			q.EventProcessor.On(e)
		}
	}
}

// stamped prefixes each write with an RFC 3339 UTC time; Compose's plain
// writer emits one line per write.
type stamped struct {
	w   io.Writer
	now func() time.Time
}

func (s stamped) Write(p []byte) (int, error) {
	_, err := fmt.Fprintf(s.w, "%s%s", s.now().UTC().Format("2006-01-02T15:04:05.000Z07:00"), p)
	return len(p), err
}

// jsonEvents is Compose's JSON progress format (cmd/display/json.go) plus
// time, project, service, replica and message.
type jsonEvents struct {
	out      io.Writer
	now      func() time.Time
	project  string
	services map[string]string // compose service (proxy or replicas) → Service
}

type jsonEvent struct {
	Time     string `json:"time"`
	Project  string `json:"project"` // "" for a summary across projects
	Service  string `json:"service,omitempty"`
	Replica  string `json:"replica,omitempty"`
	ID       string `json:"id,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Status   string `json:"status"`
	Text     string `json:"text,omitempty"`
	Details  string `json:"details,omitempty"`
	Message  string `json:"message"`
	Current  int64  `json:"current,omitempty"`
	Total    int64  `json:"total,omitempty"`
	Percent  int    `json:"percent,omitempty"`
}

// setProject tells a json display which project and Services it reports on.
func setProject(ep api.EventProcessor, r *transform.Result) {
	j, ok := ep.(*jsonEvents)
	if !ok {
		return
	}
	j.project, j.services = r.Project.Name, map[string]string{}
	for _, s := range r.Services {
		j.services[s.Name], j.services[transform.AppName(s.Name)] = s.Name, s.Name
	}
}

// forProject points a json display at one project before it is loaded
// (refresh), or at none ("") for a summary across projects.
func forProject(ep api.EventProcessor, name string) {
	if j, ok := ep.(*jsonEvents); ok {
		j.project, j.services = name, nil
	}
}

func (j *jsonEvents) Start(context.Context, string) {}
func (j *jsonEvents) Done(string, bool)             {}

func (j *jsonEvents) On(events ...api.Resource) {
	for _, e := range events {
		svc, replica := j.subject(e.ID)
		b, err := json.Marshal(jsonEvent{
			Time: j.now().UTC().Format(time.RFC3339Nano), Project: j.project, Service: svc, Replica: replica,
			ID: e.ID, ParentID: e.ParentID, Status: e.StatusText(), Text: e.Text, Details: e.Details,
			Message: strings.TrimSpace(e.Text + " " + e.Details), Current: e.Current, Total: e.Total, Percent: e.Percent,
		})
		if err == nil {
			_, _ = j.out.Write(append(b, '\n'))
		}
	}
}

// subject maps an event ID ("Service web", "Container proj-web-app-1") to
// its Service and, for a replica, the replica's container name.
func (j *jsonEvents) subject(id string) (svc, replica string) {
	kind, name, _ := strings.Cut(id, " ")
	switch kind {
	case "Service":
		return name, ""
	case "Container":
		s := strings.TrimPrefix(name, j.project+"-")
		if i := strings.LastIndexByte(s, '-'); i > 0 {
			s = s[:i] // the container number
		}
		base, ok := j.services[s]
		switch {
		case !ok:
			return s, "" // a plain service
		case s != base:
			return base, name // a replica
		}
		return base, "" // the proxy
	}
	return "", ""
}
