package cli

import (
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func stopCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var timeout int
	cmd := &cobra.Command{Use: "stop [SERVICE...]", Short: "Stop containers (dependents first); no drain",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			defer l.show(cmd.Context(), "stop")()
			opts := api.StopOptions{Project: l.Derived.Project, Services: expand(l, args)}
			if cmd.Flags().Changed("timeout") {
				d := time.Duration(timeout) * time.Second
				opts.Timeout = &d
			}
			return Exit(1, l.Compose.Stop(cmd.Context(), l.Derived.Project.Name, opts))
		}}
	cmd.Flags().IntVarP(&timeout, "timeout", "t", 0, "Seconds to wait before killing")
	return cmd
}
