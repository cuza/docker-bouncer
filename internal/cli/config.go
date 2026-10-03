package cli

import (
	"fmt"

	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

func configCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	return &cobra.Command{Use: "config", Short: "Print the derived project",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			b, err := l.Derived.Project.MarshalYAML()
			if err != nil {
				return err
			}
			// The proxy entrypoint holds $BOUNCER_* that compose would interpolate.
			_, err = fmt.Fprintf(dockerCli.Out(), "# derived by docker bouncer; not loadable by plain docker compose\n%s", b)
			return err
		}}
}
