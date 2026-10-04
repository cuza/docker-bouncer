package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

func historyCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	return &cobra.Command{Use: "history SERVICE", Short: "Show stored revisions", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			cur, err := current(cmd.Context(), l, args[0])
			if err != nil || cur == nil {
				return Exit(1, fmt.Errorf("%s: no running replica with revision labels", args[0]))
			}
			h, err := revision.History(cur)
			if err != nil {
				return Exit(1, err)
			}
			w := tabwriter.NewWriter(dockerCli.Out(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "REVISION\tTIME\tIMAGE\tUP-ID")
			for i, e := range h {
				img := "?"
				if spec, err := revision.Decode(e.Spec); err == nil {
					img = spec.Image
					// The exact image, shortened: a digest or an image ID.
					if _, h, ok := strings.Cut(spec.Labels[labelImage], "sha256:"); ok {
						img += " (" + h[:min(12, len(h))] + ")"
					}
				}
				mark := ""
				if i == 0 {
					mark = " *"
				}
				fmt.Fprintf(w, "%d%s\t%s\t%s\t%s\n", e.Revision, mark, e.Time.Format(time.RFC3339), img, e.UpID)
			}
			return w.Flush()
		}}
}
