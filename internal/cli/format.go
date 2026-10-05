package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/format"
	"github.com/cuza/docker-bouncer/internal/lock"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
)

// writeFormat is the format label new replicas get; the e2e build sets it
// (BOUNCER_E2E_FORMAT) to deploy a stack as another format would, "none"
// for one from before formats were recorded.
var writeFormat = format.Current.String()

// stamp writes the bookkeeping labels of a replica this run creates: how it
// loaded the project, and the CLI's version and storage format.
func stamp(app *types.ServiceConfig, inv invocation) {
	app.Labels[transform.LabelInvocation] = inv.encode()
	app.Labels[transform.LabelVersion], app.Labels[transform.LabelFormat] = Version, writeFormat
	if writeFormat == "none" {
		delete(app.Labels, transform.LabelVersion)
		delete(app.Labels, transform.LabelFormat)
	}
}

// versionLocker labels the lock container with the version and format too.
type versionLocker struct{ lock.Locker }

func (v versionLocker) Create(ctx context.Context, name, image string, labels map[string]string) (string, error) {
	labels = maps.Clone(labels)
	labels[transform.LabelVersion], labels[transform.LabelFormat] = Version, format.Current.String()
	return v.Locker.Create(ctx, name, image, labels)
}

// stackLabels are the labels of the project's replicas and lock: the
// containers that carry a format.
func stackLabels(ctx context.Context, c client.APIClient, project string) ([]map[string]string, error) {
	res, err := c.ContainerList(ctx, client.ContainerListOptions{All: true,
		Filters: client.Filters{}.Add("label", api.ProjectLabel+"="+project)})
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for _, ct := range res.Items {
		if transform.IsReplica(ct.Labels) || ct.Labels[transform.LabelRole] == transform.RoleLock {
			out = append(out, ct.Labels)
		}
	}
	return out, nil
}

// stackFormats is what the format labels of a stack's replicas and lock
// say. A missing label (from before formats were recorded) counts as 1.0 in
// high and low; one this CLI can't parse is unknown, and counts as newer.
type stackFormats struct {
	high, low       format.Format
	highVer, lowVer string
	missing, seen   bool
	unknown         string // the first unparseable label
	unknownVer      string
}

func scanFormats(labels []map[string]string) stackFormats {
	var s stackFormats
	for _, l := range labels {
		f, ok := labelFormat(l)
		if !ok {
			if s.unknown == "" {
				s.unknown, s.unknownVer = l[transform.LabelFormat], l[transform.LabelVersion]
			}
			continue
		}
		s.missing = s.missing || l[transform.LabelFormat] == ""
		if !s.seen || s.high.Less(f) {
			s.high, s.highVer = f, l[transform.LabelVersion]
		}
		if !s.seen || f.Less(s.low) {
			s.low, s.lowVer = f, l[transform.LabelVersion]
		}
		s.seen = true
	}
	return s
}

// refusal is why a command that rewrites Bouncer state must not touch the
// stack, or "".
func (s stackFormats) refusal(project string) string {
	switch {
	case s.unknown != "":
		return fmt.Sprintf("project %s: format %q from bouncer %s is not one this CLI understands; upgrade docker-bouncer, or pass --ignore-format", project, s.unknown, s.unknownVer)
	case !s.seen:
		return ""
	case s.high.Major > format.Major:
		return fmt.Sprintf("project %s: format %s from bouncer %s is newer than this CLI (%s) can safely change; upgrade docker-bouncer, or pass --ignore-format", project, s.high, s.highVer, Version)
	case s.low.Major < format.MinReadableMajor:
		return fmt.Sprintf("project %s: format %s is no longer read by this CLI; run docker bouncer down with bouncer %s, then up with this one", project, s.low, s.lowVer)
	}
	return ""
}

// formatCheck decides whether a command may run on a stack whose replicas
// and lock carry labels: a refusal when a command that rewrites Bouncer
// state (mutating) meets a format this CLI can't safely change or no longer
// reads; otherwise the mismatches to warn about. Read-only commands, stop
// and down only warn; --ignore-format (ignore) turns refusals into
// warnings. Versions are shown, never compared.
func formatCheck(project string, labels []map[string]string, mutating, ignore bool) (error, []api.Resource) {
	var notes []api.Resource
	warn := func(text string) {
		notes = append(notes, api.Resource{ID: "Project " + project, Status: api.Warning, Text: text})
	}
	s := scanFormats(labels)
	refuse := s.refusal(project)
	switch {
	case refuse != "" && mutating && !ignore:
		return errors.New(refuse), nil
	case refuse != "" && mutating:
		warn("--ignore-format: " + refuse)
	case refuse != "":
		warn(refuse)
	case s.high.Major == format.Major && s.high.Minor > format.Minor:
		warn(fmt.Sprintf("project %s: deployed with a newer bouncer (%s, format %s); this CLI writes %s", project, s.highVer, s.high, format.Current))
	}
	if hint := migrateHint(project, labels); hint != "" && refuse == "" {
		warn(hint)
	}
	return nil, notes
}

// migrateHint asks to migrate a stack older than the CLI's format, or, below
// the oldest format this CLI reads, to take it down and up again; "" when
// neither applies.
func migrateHint(project string, labels []map[string]string) string {
	s := scanFormats(labels)
	switch {
	case !s.seen:
		return ""
	case s.low.Major < format.MinReadableMajor:
		return s.refusal(project)
	case s.missing:
		return fmt.Sprintf("project %s is at format none, this CLI writes %s; run docker bouncer migrate %s", project, format.Current, project)
	case s.low.Less(format.Current):
		return fmt.Sprintf("project %s is at format %s, this CLI writes %s; run docker bouncer migrate %s", project, s.low, format.Current, project)
	}
	return ""
}

// labelFormat is the format a container's labels were written in: 1.0 for
// one from before formats were recorded. ok is false for a label this CLI
// can't parse (then Current, so payloads are read as they are).
func labelFormat(labels map[string]string) (format.Format, bool) {
	v := labels[transform.LabelFormat]
	if v == "" {
		return format.Format{Major: 1}, true
	}
	f, err := format.Parse(v)
	if err != nil {
		return format.Current, false
	}
	return f, true
}

// readHistory is revision.History with every stored spec migrated from the
// replica's format to the CLI's, so undo can restore an old revision.
//
// ponytail: up and undo pass the replica's labels to revision.Stamp, which
// carries the history label forward as stored (only migrate re-encodes it,
// with revision.Relabel); the first step that changes payloads must migrate
// the labels handed to Stamp too.
func readHistory(labels map[string]string) ([]revision.Entry, error) {
	h, err := revision.History(labels)
	if err != nil {
		return nil, err
	}
	var p format.Payloads
	for _, e := range h {
		p.Specs = append(p.Specs, e.Spec)
	}
	from, _ := labelFormat(labels)
	if p, err = format.Migrate(format.Steps, from, p); err != nil {
		return nil, err
	}
	for i := range h {
		h[i].Spec = p.Specs[i]
	}
	return h, nil
}

// checkFormat runs formatCheck on l's project before a command touches it,
// and shows its notes. A mutating command checks again once it holds the
// lock (l.recheck), in case another run rewrote the stack in between.
func checkFormat(ctx context.Context, dockerCli command.Cli, l *loaded, mutating, ignore bool) error {
	if mutating {
		l.recheck = func(ctx context.Context) error {
			return refuseFormat(ctx, dockerCli, l.Derived.Project.Name, ignore, 2)
		}
	}
	labels, err := stackLabels(ctx, dockerCli.Client(), l.Derived.Project.Name)
	if err != nil {
		return Exit(1, err)
	}
	refuse, notes := formatCheck(l.Derived.Project.Name, labels, mutating, ignore)
	if refuse != nil {
		return Exit(2, refuse)
	}
	if len(notes) == 0 {
		return nil
	}
	l.Events.Start(ctx, "format")
	l.Events.On(notes...)
	l.Events.Done("format", true)
	return nil
}

// refuseFormat is the mutating format check alone, as an exit with code.
func refuseFormat(ctx context.Context, dockerCli command.Cli, project string, ignore bool, code int) error {
	labels, err := stackLabels(ctx, dockerCli.Client(), project)
	if err != nil {
		return Exit(1, err)
	}
	refuse, _ := formatCheck(project, labels, true, ignore)
	return Exit(code, refuse)
}

// e2eLockDelay holds every run before it takes the lock; only the e2e
// build sets it (BOUNCER_E2E_LOCK_DELAY), to race another run.
var e2eLockDelay time.Duration
