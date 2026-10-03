package main

import (
	"github.com/cuza/docker-bouncer/internal/cli"
	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/docker/cli/cli-plugins/plugin"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

var version = "dev" // -ldflags "-X main.version=…" at release

func main() {
	cli.Version = version
	plugin.Run(func(dockerCli command.Cli) *cobra.Command {
		root := cli.NewRoot(dockerCli)
		root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return cli.Exit(2, err) })
		return root
	}, metadata.Metadata{
		SchemaVersion:    "0.1.0",
		Vendor:           "cuza",
		Version:          version,
		ShortDescription: "Services and rolling bounces for Compose",
	})
}
