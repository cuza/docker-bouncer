package cli

import (
	"fmt"

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
			fmt.Fprintln(dockerCli.Err(), "warning: revision history lives on the replicas and is deleted with them")
			return Exit(1, l.Compose.Down(cmd.Context(), l.Derived.Project.Name,
				api.DownOptions{Project: l.Derived.Project, RemoveOrphans: true}))
		}}
}
