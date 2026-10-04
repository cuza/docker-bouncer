package cli

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
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
			all := append(res.Items, locks.Items...)
			if err := lsTable(dockerCli.Out(), all); err != nil {
				return err
			}
			for _, h := range lsHints(all) {
				fmt.Fprintln(dockerCli.Err(), h)
			}
			return nil
		}}
}

// lsHints are the format warnings each project gets from a read-only
// command: a newer or unreadable format, or a hint to migrate.
func lsHints(cs []container.Summary) []string {
	stacks := map[string][]map[string]string{}
	for _, c := range cs {
		if r := c.Labels[transform.LabelRole]; r == transform.RoleReplica || r == transform.RoleLock {
			p := c.Labels[api.ProjectLabel]
			stacks[p] = append(stacks[p], c.Labels)
		}
	}
	var out []string
	for _, p := range slices.Sorted(maps.Keys(stacks)) {
		_, notes := formatCheck(p, stacks[p], false, false)
		for _, n := range notes {
			out = append(out, n.Text)
		}
	}
	return out
}

// lsTable has a row per Service of the Bouncer containers and locks in cs.
// CONFIG FILES is Compose's label, comma separated, so a script can run `up`
// on every project without knowing where the files live.
func lsTable(out io.Writer, cs []container.Summary) error {
	locked := map[string]bool{}
	files := map[string]string{}
	type row struct {
		rev, total, happy, running int
		hashes                     map[string]bool
		version, format            string // of the newest replicas
	}
	rows := map[[2]string]*row{}
	for _, c := range cs {
		project := c.Labels[api.ProjectLabel]
		if f := c.Labels[api.ConfigFilesLabel]; f != "" {
			files[project] = f
		}
		switch c.Labels[transform.LabelRole] {
		case transform.RoleLock:
			locked[project] = true
			continue
		case transform.RoleReplica:
		default:
			continue
		}
		k := [2]string{project, c.Labels[transform.LabelService]}
		r := rows[k]
		if r == nil {
			r = &row{hashes: map[string]bool{}}
			rows[k] = r
		}
		r.total++
		if string(c.State) == "running" {
			r.running++
			if !strings.Contains(c.Status, "unhealthy") && !strings.Contains(c.Status, "starting") {
				r.happy++
			}
		}
		if n, _ := strconv.Atoi(c.Labels[revision.LabelRevision]); n > r.rev || r.version == "" {
			r.rev = max(r.rev, n)
			r.version, r.format = c.Labels[transform.LabelVersion], c.Labels[transform.LabelFormat]
		}
		r.hashes[c.Labels[revision.LabelSpecHash]] = true
	}
	keys := make([][2]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"/"+keys[i][1] < keys[j][0]+"/"+keys[j][1] })
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSERVICE\tREVISION\tREPLICAS\tSTATUS\tVERSION\tFORMAT\tCONFIG FILES")
	for _, k := range keys {
		r := rows[k]
		status := "converged"
		switch {
		case locked[k[0]]:
			status = "bouncing"
		case r.running == 0: // before drifted: no replica running is stopped, whatever revisions remain
			status = "stopped"
		case len(r.hashes) > 1:
			status = "drifted"
		}
		f := files[k[0]]
		if f == "" {
			f = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d/%d\t%s\t%s\t%s\t%s\n", k[0], k[1], r.rev, r.happy, r.total, status, cmp.Or(r.version, "-"), cmp.Or(r.format, "-"), f)
	}
	return w.Flush()
}
