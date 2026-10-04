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
		if r := ct.Labels[transform.LabelRole]; r == transform.RoleReplica || r == transform.RoleLock {
			out = append(out, ct.Labels)
		}
	}
	return out, nil
}

// formatCheck decides whether a command may run on a stack whose replicas
// and lock carry labels: a refusal when a command that rewrites
// Bouncer state (mutating) meets a major this CLI can't safely change or
// no longer reads; otherwise notes to show. Read-only commands, stop and
// down only warn; --ignore-format (ignore) turns refusals into warnings.
// Versions are shown, never compared.
func formatCheck(project string, labels []map[string]string, mutating, ignore bool) (error, []api.Resource) {
	id := "Project " + project
	var notes []api.Resource
	note := func(st api.EventStatus, text string) {
		notes = append(notes, api.Resource{ID: id, Status: st, Text: text})
	}
	var high, low format.Format
	var highVer, lowVer string
	missing, seen := false, false
	for _, l := range labels {
		f, err := format.Parse(l[transform.LabelFormat])
		if l[transform.LabelFormat] == "" || err != nil {
			missing, f = true, format.Format{Major: 1} // before formats were recorded: 1.0
		}
		if !seen || high.Less(f) {
			high, highVer = f, l[transform.LabelVersion]
		}
		if !seen || f.Less(low) {
			low, lowVer = f, l[transform.LabelVersion]
		}
		seen = true
	}
	var refuse string
	switch {
	case !seen:
		return nil, nil
	case high.Major > format.Major:
		refuse = fmt.Sprintf("project %s: format %s from bouncer %s is newer than this CLI (%s) can safely change; upgrade docker-bouncer, or pass --ignore-format", project, high, highVer, Version)
	case low.Major < format.MinReadableMajor:
		refuse = fmt.Sprintf("project %s: format %s is no longer read by this CLI; run docker bouncer down with bouncer %s, then up with this one", project, low, lowVer)
	case high.Major == format.Major && high.Minor > format.Minor:
		note(api.Warning, fmt.Sprintf("deployed with a newer bouncer (%s, format %s); this CLI writes %s", highVer, high, format.Current))
	}
	switch {
	case refuse != "" && mutating && !ignore:
		return errors.New(refuse), nil
	case refuse != "" && mutating:
		note(api.Warning, "--ignore-format: "+refuse)
	case refuse != "":
		note(api.Warning, refuse)
	}
	if missing {
		note(api.Done, "deployed before bouncer recorded its version; the next bounce records it")
	}
	return nil, notes
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
