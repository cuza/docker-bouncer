package cli

import (
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

func pullCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	return &cobra.Command{Use: "pull [SERVICE...]", Short: "Pull images, including the proxy image",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			defer l.show(cmd.Context(), "pull")()
			if len(args) > 0 {
				p, err := l.Derived.Project.WithSelectedServices(expand(l, args), types.IgnoreDependencies)
				if err != nil {
					return Exit(2, err)
				}
				l.Derived.Project = p
			}
			return Exit(2, prePull(cmd.Context(), l, l.Derived.Project.Services, types.PullPolicyAlways, false, imagePresent(dockerCli)))
		}}
}
