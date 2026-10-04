package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func psCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	return &cobra.Command{Use: "ps", Short: "Containers with role and revision",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			l, err := load(ctx, dockerCli, pf)
			if err != nil {
				return err
			}
			want := map[string]string{} // service → desired spec hash
			listed := map[string]map[string]bool{}
			for _, s := range append(l.Derived.Services, l.Derived.Disabled...) {
				app, ok := l.Derived.Project.Services[transform.AppName(s.Name)]
				if !ok {
					app = l.Derived.Project.DisabledServices[transform.AppName(s.Name)]
				}
				if pinned, err := pinBuilt(ctx, l, app); err == nil {
					app = pinned
				}
				_, h, _ := revision.Encode(app)
				want[s.Name] = h
				px, _ := l.Engine.Container(ctx, l.Derived.Project.Name, map[string]string{transform.LabelRole: transform.RoleProxy, transform.LabelService: s.Name})
				if px != nil && px.Running {
					listed[s.Name], _ = engine.NewProxy(l.Engine, px.ID, s).Health(ctx)
				}
			}
			cs, err := l.Compose.Ps(ctx, l.Derived.Project.Name, api.PsOptions{All: true, Project: l.Derived.Project})
			if err != nil {
				return Exit(1, err)
			}
			w := tabwriter.NewWriter(dockerCli.Out(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSERVICE\tROLE\tREVISION\tIMAGE\tSTATE\tHEALTH")
			for _, c := range cs {
				role, rev := "-", "-"
				switch c.Labels[transform.LabelRole] {
				case transform.RoleProxy:
					role = "proxy"
				case transform.RoleReplica:
					svc := c.Labels[transform.LabelService]
					rev = c.Labels[revision.LabelRevision]
					_, inList := listed[svc][c.Name]
					switch {
					case c.Labels[revision.LabelSpecHash] != want[svc] && inList:
						role = "old"
					case c.Labels[revision.LabelSpecHash] != want[svc]:
						role = "draining"
					case !inList:
						role = "new"
					default:
						role = "live"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Service, role, rev, c.Image, c.State, c.Health)
			}
			return w.Flush()
		}}
}
