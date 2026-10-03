package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

func lsCmd(dockerCli command.Cli) *cobra.Command {
	return &cobra.Command{Use: "ls", Short: "Projects with bouncer Services on this host",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			res, err := dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{All: true,
				Filters: client.Filters{}.Add("label", transform.LabelManaged+"=true")})
			if err != nil {
				return Exit(1, err)
			}
			locks, err := dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{All: true,
				Filters: client.Filters{}.Add("label", transform.LabelRole+"="+transform.RoleLock)})
			if err != nil {
				return Exit(1, err)
			}
			locked := map[string]bool{}
			for _, c := range locks.Items {
				locked[c.Labels[api.ProjectLabel]] = true
			}
			type row struct {
				rev, total, happy int
				hashes            map[string]bool
			}
			rows := map[[2]string]*row{}
			for _, c := range res.Items {
				if c.Labels[transform.LabelRole] != transform.RoleReplica {
					continue
				}
				k := [2]string{c.Labels[api.ProjectLabel], c.Labels[transform.LabelService]}
				r := rows[k]
				if r == nil {
					r = &row{hashes: map[string]bool{}}
					rows[k] = r
				}
				r.total++
				if string(c.State) == "running" && !strings.Contains(c.Status, "unhealthy") && !strings.Contains(c.Status, "starting") {
					r.happy++
				}
				if n, _ := strconv.Atoi(c.Labels[revision.LabelRevision]); n > r.rev {
					r.rev = n
				}
				r.hashes[c.Labels[revision.LabelSpecHash]] = true
			}
			keys := make([][2]string, 0, len(rows))
			for k := range rows {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"/"+keys[i][1] < keys[j][0]+"/"+keys[j][1] })
			w := tabwriter.NewWriter(dockerCli.Out(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tSERVICE\tREVISION\tREPLICAS\tSTATUS")
			for _, k := range keys {
				r := rows[k]
				status := "converged"
				switch {
				case locked[k[0]]:
					status = "bouncing"
				case len(r.hashes) > 1:
					status = "drifted"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d/%d\t%s\n", k[0], k[1], r.rev, r.happy, r.total, status)
			}
			return w.Flush()
		}}
}
