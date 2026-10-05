package cli

import (
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/cmd/formatter"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/spf13/cobra"
)

func logsCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var follow, proxy bool
	var tail string
	cmd := &cobra.Command{Use: "logs SERVICE", Short: "Logs of all replicas (or the proxy)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(cmd.Context(), dockerCli, pf)
			if err != nil {
				return err
			}
			if err := checkFormat(cmd.Context(), dockerCli, l, false, pf.IgnoreFormat); err != nil {
				return err
			}
			target := args[0]
			if !proxy {
				if _, ok := l.Derived.Project.Services[transform.AppName(target)]; ok {
					target = transform.AppName(target)
				}
			}
			consumer := formatter.NewLogConsumer(cmd.Context(), dockerCli.Out(), dockerCli.Err(), true, true, false)
			return l.Compose.Logs(cmd.Context(), l.Derived.Project.Name, consumer,
				api.LogOptions{Project: l.Derived.Project, Services: []string{target}, Follow: follow, Tail: tail})
		}}
	cmd.Flags().BoolVar(&follow, "follow", false, "Follow log output") // -f is --file
	cmd.Flags().BoolVar(&proxy, "proxy", false, "Show the Envoy proxy's logs")
	cmd.Flags().StringVarP(&tail, "tail", "n", "all", "Lines from the end")
	return cmd
}
