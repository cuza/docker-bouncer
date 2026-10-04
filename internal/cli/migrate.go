package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/format"
	"github.com/cuza/docker-bouncer/internal/lock"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func migrateCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate [PROJECT...]",
		Short: "Bring projects stored in an older format to this CLI's",
		Long: `Bring projects stored in an older format to this CLI's.

For every project on this host with Bouncer replicas, or the named ones,
whose replicas or lock are at an older storage format (or none): the stored
specs, history and invocation go through the migration chain, then every
Service bounces from its current revision (its exact image; no build, no
pull) so the new replicas carry the new format. Plain services are left
alone. A project already at this CLI's format is up to date; one at a newer
format is refused (no downgrades). A project from before invocations were
recorded is loaded from the current directory and -f, like up.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ev, err := newEvents(dockerCli, pf.Progress, pf.Timestamps)
			if err != nil {
				return err
			}
			if pf.Name != "" {
				args = append(args, pf.Name)
			}
			found, err := bouncerProjects(ctx, dockerCli.Client())
			if err != nil {
				return Exit(1, err)
			}
			names, err := pickProjects(found, args)
			if err != nil {
				return err
			}
			ev.Start(ctx, "migrate")
			defer ev.Done("migrate", true)
			var res []refreshResult
			for _, name := range names {
				forProject(ev, name)
				res = append(res, migrateProject(ctx, dockerCli, ev, name, found[name], pf, dryRun))
			}
			forProject(ev, "")
			text, err := refreshSummary("migrate", res)
			status := api.Done
			if err != nil {
				status = api.Error
			}
			ev.On(api.Resource{ID: "Migrate", Status: status, Text: text})
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show the steps and the Services that would bounce; change nothing")
	return cmd
}

// migrateCheck decides what migrate does with a stack: an error (exit 2)
// when it is newer than the CLI (no downgrades), unknown, or below the
// oldest readable format; atCurrent when it is at the CLI's format; else the
// format it migrates from ("none" without labels).
func migrateCheck(project string, labels []map[string]string) (from string, atCurrent bool, err error) {
	s := scanFormats(labels)
	switch {
	case s.unknown != "":
		return "", false, fmt.Errorf("format %q from bouncer %s is not one this CLI understands; upgrade docker-bouncer", s.unknown, s.unknownVer)
	case format.Current.Less(s.high):
		return "", false, fmt.Errorf("format %s from bouncer %s is newer than this CLI (%s); migrate never downgrades, upgrade docker-bouncer", s.high, s.highVer, Version)
	case s.seen && s.low.Major < format.MinReadableMajor:
		return "", false, errors.New(s.refusal(project))
	case !s.missing && !s.low.Less(format.Current):
		return "", true, nil
	case s.missing:
		return "none", false, nil
	}
	return s.low.String(), false, nil
}

func migrateProject(ctx context.Context, dockerCli command.Cli, ev api.EventProcessor, name string, hp hostProject, pf *ProjectFlags, dryRun bool) refreshResult {
	say := func(st api.EventStatus, text string, details ...string) {
		ev.On(api.Resource{ID: "Project " + name, Status: st, Text: text, Details: strings.Join(details, " ")})
	}
	skip := func(why string) refreshResult {
		say(api.Warning, "Skipped:", why)
		return refreshResult{status: skipped}
	}
	fail := func(code int, err error) refreshResult {
		say(api.Error, "Failed: "+err.Error())
		return refreshResult{status: failed, err: Exit(code, err)}
	}
	if !hp.running {
		return skip("stopped; `docker bouncer up` starts it, then migrate")
	}
	labels, err := stackLabels(ctx, dockerCli.Client(), name)
	if err != nil {
		return fail(1, err)
	}
	from, atCurrent, err := migrateCheck(name, labels)
	switch {
	case err != nil:
		return fail(2, err)
	case atCurrent:
		say(api.Done, "Up to date", "format "+format.Current.String())
		return refreshResult{status: upToDate}
	}
	var steps []string // the chain's steps; a relabel when there are none
	low, _ := format.Parse(from)
	for _, s := range format.Steps {
		if from == "none" || !s.From.Less(low) {
			steps = append(steps, s.From.String()+" → "+s.To.String())
		}
	}
	if len(steps) == 0 {
		steps = []string{from + " → " + format.Current.String()}
	}
	flags := pf
	inv, ok, err := decodeInvocation(hp.labels)
	if err != nil {
		return fail(1, fmt.Errorf("invocation label: %w", err))
	}
	if ok {
		flags = inv.flags(pf.Progress, pf.Timestamps)
	}
	l, err := loadWith(ctx, dockerCli, flags, ev)
	if err != nil {
		return fail(2, err)
	}
	if l.Derived.Project.Name != name {
		return fail(2, fmt.Errorf("no invocation recorded; run docker bouncer migrate %s from its project directory, or with -f", name))
	}
	var svcs []config.Service // those with a running replica to bounce from
	var names []string
	for _, svc := range l.Derived.Services {
		cur, err := current(ctx, l, svc.Name)
		if err != nil {
			return fail(1, err)
		}
		if cur == nil {
			l.event("Service "+svc.Name, api.Warning, "Skipped:", "no running replica; `docker bouncer up` starts it, then migrate")
			continue
		}
		svcs, names = append(svcs, svc), append(names, svc.Name)
	}
	if len(svcs) == 0 {
		return skip("no Service has a running replica")
	}
	if dryRun {
		say(api.Warning, "Would migrate:", strings.Join(steps, ", ")+";", "would bounce "+strings.Join(names, ", "))
		return refreshResult{status: wouldDo}
	}
	say(api.Working, "Migrating:", strings.Join(steps, ", "))
	l.recheck = func(ctx context.Context) error { // under the lock: has another run changed the format?
		labels, err := stackLabels(ctx, dockerCli.Client(), name)
		if err == nil {
			_, _, err = migrateCheck(name, labels)
		}
		return Exit(2, err)
	}
	l.key = func(labels map[string]string) string { // same spec, older format: replaced
		return labels[revision.LabelSpecHash] + " " + labels[transform.LabelFormat]
	}
	err = withLock(ctx, dockerCli, l, false, func() error {
		return bounceAll(ctx, l, svcs, func(svc config.Service) (types.ServiceConfig, error) {
			cur, err := current(ctx, l, svc.Name) // re-read under the lock
			if err == nil && cur == nil {
				err = fmt.Errorf("%s: no running replica with revision labels", svc.Name)
			}
			if err != nil {
				return types.ServiceConfig{}, err
			}
			return migratedApp(l, svc, cur)
		})
	})
	if held, ok := errors.AsType[lock.ErrHeld](err); ok {
		return skip("bounce in progress by " + held.Owner)
	}
	if err != nil {
		return fail(1, err)
	}
	say(api.Done, "Migrated", "to format "+format.Current.String())
	return refreshResult{status: refreshed}
}

// migratedApp is a Service's current revision, from the replica labels cur,
// as this CLI stores it: the spec, history and invocation run through the
// migration chain and re-encoded, run from its exact image, with this CLI's
// version and format.
func migratedApp(l *loaded, svc config.Service, cur map[string]string) (types.ServiceConfig, error) {
	h, err := readHistory(cur)
	if err != nil {
		return types.ServiceConfig{}, err
	}
	app, err := restore(l, svc, h[0])
	if err != nil {
		return app, err
	}
	app.Image = runImage(app)
	labels, err := revision.Relabel(cur, h)
	if err != nil {
		return app, err
	}
	app.Labels = maps.Clone(app.Labels)
	for k, v := range labels {
		if strings.HasPrefix(k, transform.LabelPrefix) {
			app.Labels[k] = v
		}
	}
	inv, ok, err := decodeInvocation(cur)
	if err != nil || !ok {
		inv = l.inv
	}
	stamp(&app, inv) // re-encoded; keeps an undo hold when there is one
	return app, nil
}
