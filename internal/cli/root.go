package cli

import (
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

// addCommands grows as commands land (Tasks 10–11).
func addCommands(root *cobra.Command, dockerCli command.Cli, pf *ProjectFlags) {}
