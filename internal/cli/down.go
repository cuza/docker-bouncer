package cli

import (
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func downCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	return &cobra.Command{Use: "down", Short: "Remove the project (deletes revision history)",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			defer l.show(cmd.Context(), "down")()
			l.event("Project "+l.Derived.Project.Name, api.Warning, "Deleting revision history:", "it lives on the replicas")
			// Every profile: a Service up with --profile goes too, proxy and replicas.
			p, err := l.Derived.Project.WithProfiles([]string{"*"})
			if err != nil {
				return Exit(1, err)
			}
			return Exit(1, l.Compose.Down(cmd.Context(), p.Name, api.DownOptions{Project: p, RemoveOrphans: true}))
		}}
}
