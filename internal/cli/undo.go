package cli

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func undoCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var to int
	cmd := &cobra.Command{Use: "undo [SERVICE]", Short: "Bounce back to a stored revision", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			l, err := load(ctx, dockerCli, pf)
			if err != nil {
				return err
			}
			defer l.show(ctx, "undo")()
			if len(args) > 0 && len(selected(l.Derived.Services, args)) == 0 {
				return configErr("%s is not a Service", args[0])
			}
			history := map[string][]revision.Entry{}
			latest := map[string]revision.Entry{}
			for _, svc := range selected(l.Derived.Services, args) {
				cur, err := current(ctx, l, svc.Name)
				if err != nil {
					return Exit(1, err)
				}
				if cur == nil {
					continue
				}
				h, err := revision.History(cur)
				if err != nil {
					return Exit(1, fmt.Errorf("%s: %w", svc.Name, err))
				}
				history[svc.Name], latest[svc.Name] = h, h[0]
			}
			if len(latest) == 0 {
				return Exit(1, fmt.Errorf("no running replica with revision labels to undo"))
			}
			// Restore before the lock: the targets' images are pulled now, never mid-bounce.
			type plan struct {
				app    types.ServiceConfig
				target revision.Entry
				file   int
			}
			plans := map[string]plan{}
			pulls := types.Services{}
			names := servicesOfLastUp(latest)
			for _, svc := range selected(l.Derived.Services, names) {
				h := history[svc.Name]
				if len(args) == 0 && len(h) < 2 {
					l.event("Service "+svc.Name, api.Warning, "Nothing to undo:", "first revision")
					continue
				}
				target, err := undoTarget(h, to)
				if err != nil {
					return Exit(1, fmt.Errorf("%s: %w", svc.Name, err))
				}
				app, err := restore(l, svc, target)
				if err != nil {
					return Exit(1, fmt.Errorf("%s: revision %d: %w", svc.Name, target.Revision, err))
				}
				plans[svc.Name] = plan{app, target, fileRevision(l, svc, h)}
				pulls[app.Name] = app
			}
			if len(plans) == 0 {
				return Exit(1, fmt.Errorf("nothing to undo"))
			}
			if err := prePull(ctx, l, pulls, "", imagePresent(dockerCli)); err != nil {
				return Exit(2, err)
			}
			return withLock(ctx, dockerCli, l, false, func() error {
				upID := time.Now().UTC().Format("20060102T150405Z")
				var svcs []config.Service
				for _, svc := range l.Derived.Services {
					if _, ok := plans[svc.Name]; ok {
						svcs = append(svcs, svc)
					}
				}
				return bounceAll(ctx, l, svcs, func(svc config.Service) (types.ServiceConfig, error) {
					cur, err := current(ctx, l, svc.Name) // re-read under the lock
					if err == nil && cur == nil {
						err = fmt.Errorf("%s: no running replica with revision labels", svc.Name)
					}
					if err != nil {
						return types.ServiceConfig{}, err
					}
					p := plans[svc.Name]
					// A concurrent up may have moved history since the target was picked.
					h, err := revision.History(cur)
					if err != nil {
						return types.ServiceConfig{}, err
					}
					if t, err := undoTarget(h, to); err != nil || !bytes.Equal(t.Spec, p.target.Spec) {
						return types.ServiceConfig{}, fmt.Errorf("%s: revisions changed while undo started; run it again", svc.Name)
					}
					app, kept, err := revision.Stamp(p.app, cur, upID, svc.Spec.HistoryMax, time.Now())
					if err == nil {
						warnTrimmed(l, svc, kept)
						l.event("Service "+svc.Name, api.Warning, fmt.Sprintf("Revision %s = copy of %d:", app.Labels[revision.LabelRevision], p.target.Revision),
							fmt.Sprintf("the compose file still describes revision %d, the next up rolls forward", p.file))
					}
					return app, err
				})
			})
		}}
	cmd.Flags().IntVar(&to, "to-revision", 0, "Revision to restore (default: the previous one)")
	return cmd
}

// restore rebuilds a stored revision's replica config: env_files are read
// now, and the replica count is the file's (specs carry none).
func restore(l *loaded, svc config.Service, e revision.Entry) (types.ServiceConfig, error) {
	app, err := revision.Decode(e.Spec)
	if err != nil {
		return app, err
	}
	app.Name = transform.AppName(svc.Name) // not in the JSON
	app, err = revision.Restore(app, l.Project.Environment)
	if err != nil {
		return app, err
	}
	file := l.Derived.Project.Services[app.Name]
	app.SetScale(file.GetScale())
	return app, nil
}

// fileRevision is the newest stored revision the compose file describes; the
// current one when the file matches none.
func fileRevision(l *loaded, svc config.Service, h []revision.Entry) int {
	spec, _, err := revision.Encode(l.Derived.Project.Services[transform.AppName(svc.Name)])
	if err == nil {
		for _, e := range h {
			if bytes.Equal(e.Spec, spec) {
				return e.Revision
			}
		}
	}
	return h[0].Revision
}

func undoTarget(h []revision.Entry, to int) (revision.Entry, error) {
	if len(h) < 2 {
		return revision.Entry{}, fmt.Errorf("no previous revision to undo to")
	}
	if to == 0 {
		return h[1], nil
	}
	if to == h[0].Revision {
		return revision.Entry{}, fmt.Errorf("revision %d is already running", to)
	}
	for _, e := range h[1:] {
		if e.Revision == to {
			return e, nil
		}
	}
	return revision.Entry{}, fmt.Errorf("revision %d is not in the stored history", to)
}

// servicesOfLastUp: services whose current revision came from the newest up-id
// (up-ids are UTC timestamps, so they sort).
func servicesOfLastUp(latest map[string]revision.Entry) []string {
	newest := ""
	for _, e := range latest {
		if e.UpID > newest {
			newest = e.UpID
		}
	}
	var out []string
	for name, e := range latest {
		if e.UpID == newest {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
