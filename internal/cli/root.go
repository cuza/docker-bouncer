package cli

import (
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/cuza/docker-bouncer/internal/config"
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
	// IgnoreFormat turns format refusals into warnings (see formatCheck).
	IgnoreFormat bool
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
	f.BoolVar(&pf.IgnoreFormat, "ignore-format", false, "Change a project even when its storage format is one this CLI can't safely change")
	root.SetVersionTemplate("docker bouncer {{.Version}}\n")
	addCommands(root, dockerCli, pf)
	return root
}

// addCommands registers every subcommand.
func addCommands(root *cobra.Command, dockerCli command.Cli, pf *ProjectFlags) {
	root.AddCommand(upCmd(dockerCli, pf), pullCmd(dockerCli, pf), stopCmd(dockerCli, pf), downCmd(dockerCli, pf), configCmd(dockerCli, pf),
		historyCmd(dockerCli, pf), logsCmd(dockerCli, pf), psCmd(dockerCli, pf), lsCmd(dockerCli), undoCmd(dockerCli, pf), refreshCmd(dockerCli, pf), migrateCmd(dockerCli, pf), versionCmd())
}

func versionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show the docker bouncer version and what it is built on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := "docker bouncer " + Version + "\n"
			if short {
				out = Version + "\n"
			} else {
				out += "  compose: " + moduleVersion("github.com/docker/compose/v5") + "\n"
				out += "  envoy:   " + config.DefaultProxyImage + " (default proxy image)\n"
			}
			_, err := cmd.OutOrStdout().Write([]byte(out))
			return err
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "Print only the version number")
	return cmd
}

// moduleVersion reads a dependency's version from the build info the Go
// toolchain embeds in the binary.
func moduleVersion(path string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == path {
				return d.Version
			}
		}
	}
	return "unknown"
}
