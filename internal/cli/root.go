package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/docker/cli/cli-plugins/plugin"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

// Version is set by main from the release build's -ldflags.
var Version = "dev"

type ProjectFlags struct {
	Files    []string
	Name     string
	Dir      string
	Profiles []string
	EnvFiles []string
	// Progress and Timestamps pick the display (see newEvents).
	Progress   string
	Timestamps bool
}

func NewRoot(dockerCli command.Cli) *cobra.Command {
	pf := &ProjectFlags{}
	root := &cobra.Command{
		Use:           "bouncer",
		Short:         "Services and rolling bounces for Compose projects on one host",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
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
	f.StringVar(&pf.Progress, "progress", "auto", `Progress output: "auto" (tty on a terminal, else plain), "tty", "plain", "json", "quiet" (errors only)`)
	f.BoolVar(&pf.Timestamps, "timestamps", false, "Prefix plain progress lines with an RFC 3339 UTC time (json always has one)")
	root.SetVersionTemplate("docker bouncer {{.Version}}\n")
	addCommands(root, dockerCli, pf)
	return root
}

// addCommands registers every subcommand.
func addCommands(root *cobra.Command, dockerCli command.Cli, pf *ProjectFlags) {
	root.AddCommand(upCmd(dockerCli, pf), pullCmd(dockerCli, pf), stopCmd(dockerCli, pf), downCmd(dockerCli, pf), configCmd(dockerCli, pf),
		historyCmd(dockerCli, pf), logsCmd(dockerCli, pf), psCmd(dockerCli, pf), lsCmd(dockerCli), undoCmd(dockerCli, pf), versionCmd())
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the docker bouncer version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte("docker bouncer " + Version + "\n"))
			return err
		},
	}
}
