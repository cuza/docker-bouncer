package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/docker/cli/cli-plugins/plugin"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

type ProjectFlags struct {
	Files    []string
	Name     string
	Dir      string
	Profiles []string
	EnvFiles []string
}

func NewRoot(dockerCli command.Cli) *cobra.Command {
	pf := &ProjectFlags{}
	root := &cobra.Command{
		Use:           "bouncer",
		Short:         "Services and rolling bounces for Compose projects on one host",
		SilenceUsage:  true,
		SilenceErrors: true,
		// On a TTY the docker CLI leaves SIGINT to the plugin; Go's default
		// action would kill it before the lock is released. Cobra runs only
		// the nearest PersistentPreRunE, so the plugin's hook is chained.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			go func() { <-ctx.Done(); stop() }() // a second Ctrl-C kills
			cmd.SetContext(ctx)
			if plugin.PersistentPreRunE == nil { // unit tests: no plugin.Run
				return nil
			}
			return plugin.PersistentPreRunE(cmd, args)
		},
	}
	f := root.PersistentFlags()
	f.StringArrayVarP(&pf.Files, "file", "f", nil, "Compose configuration files")
	f.StringVarP(&pf.Name, "project-name", "p", "", "Project name")
	f.StringVar(&pf.Dir, "project-directory", "", "Working directory")
	f.StringArrayVar(&pf.Profiles, "profile", nil, "Profiles to enable")
	f.StringArrayVar(&pf.EnvFiles, "env-file", nil, "Environment files")
	addCommands(root, dockerCli, pf)
	return root
}

// addCommands registers every subcommand.
func addCommands(root *cobra.Command, dockerCli command.Cli, pf *ProjectFlags) {
	root.AddCommand(upCmd(dockerCli, pf), pullCmd(dockerCli, pf), stopCmd(dockerCli, pf), downCmd(dockerCli, pf), configCmd(dockerCli, pf),
		historyCmd(dockerCli, pf), logsCmd(dockerCli, pf), psCmd(dockerCli, pf), lsCmd(dockerCli), undoCmd(dockerCli, pf))
}
