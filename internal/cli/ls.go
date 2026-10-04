package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

func lsCmd(dockerCli command.Cli) *cobra.Command {
	return &cobra.Command{Use: "ls", Short: "Compose projects on this host, with their Services",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := dockerCli.Client().ContainerList(cmd.Context(), client.ContainerListOptions{All: true,
				Filters: client.Filters{}.Add("label", api.ProjectLabel)})
			if err != nil {
				return Exit(1, err)
			}
			return lsTable(dockerCli.Out(), res.Items)
		}}
}

// lsTable has a row per Service, and one row with SERVICE "-" for a project
// that has only plain services. CONFIG FILES is Compose's label, comma
// separated, so a script can run `up` on every project without knowing where
// the files live.
func lsTable(out io.Writer, cs []container.Summary) error {
	type project struct {
		files           string
		locked, running bool
		services        bool
	}
	type row struct {
		rev, total, happy, running int
		hashes                     map[string]bool
	}
	projects := map[string]*project{}
	rows := map[[2]string]*row{}
	for _, c := range cs {
		name := c.Labels[api.ProjectLabel]
		p := projects[name]
		if p == nil {
			p = &project{}
			projects[name] = p
		}
		if f := c.Labels[api.ConfigFilesLabel]; f != "" {
			p.files = f
		}
		running := string(c.State) == "running"
		switch c.Labels[transform.LabelRole] {
		case transform.RoleLock:
			p.locked = true
			continue
		case transform.RoleReplica:
		default:
			p.running = p.running || running
			continue
		}
		p.services = true
		k := [2]string{name, c.Labels[transform.LabelService]}
		r := rows[k]
		if r == nil {
			r = &row{hashes: map[string]bool{}}
			rows[k] = r
		}
		r.total++
		if running {
			r.running++
			if !strings.Contains(c.Status, "unhealthy") && !strings.Contains(c.Status, "starting") {
				r.happy++
			}
		}
		if n, _ := strconv.Atoi(c.Labels[revision.LabelRevision]); n > r.rev {
			r.rev = n
		}
		r.hashes[c.Labels[revision.LabelSpecHash]] = true
	}
	for name, p := range projects {
		if !p.services {
			rows[[2]string{name, "-"}] = nil
		}
	}
	keys := make([][2]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"/"+keys[i][1] < keys[j][0]+"/"+keys[j][1] })
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSERVICE\tREVISION\tREPLICAS\tSTATUS\tCONFIG FILES")
	for _, k := range keys {
		p, r := projects[k[0]], rows[k]
		files := p.files
		if files == "" {
			files = "-"
		}
		if r == nil {
			status := "stopped"
			switch {
			case p.locked:
				status = "bouncing"
			case p.running:
				status = "running"
			}
			fmt.Fprintf(w, "%s\t-\t-\t-\t%s\t%s\n", k[0], status, files)
			continue
		}
		status := "converged"
		switch {
		case p.locked:
			status = "bouncing"
		case len(r.hashes) > 1:
			status = "drifted"
		case r.running == 0:
			status = "stopped"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d/%d\t%s\t%s\n", k[0], k[1], r.rev, r.happy, r.total, status, files)
	}
	return w.Flush()
}
