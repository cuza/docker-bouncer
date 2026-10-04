package cli

import (
	"maps"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/transform"
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
			defer l.show(cmd.Context(), "down")()
			l.event("Project "+l.Derived.Project.Name, api.Warning, "Deleting revision history:", "it lives on the replicas")
			p := downProject(l.Derived)
			return Exit(1, l.Compose.Down(cmd.Context(), p.Name, api.DownOptions{Project: p, RemoveOrphans: true}))
		}}
}

// downProject is the active project plus the proxy and replicas of every
// Service in an inactive profile, so none is left behind. Plain services in
// inactive profiles stay disabled: Compose keeps them running and does not
// count them as orphans, as docker compose down does.
func downProject(d *transform.Result) *types.Project {
	p := *d.Project
	p.Services, p.DisabledServices = maps.Clone(p.Services), maps.Clone(p.DisabledServices)
	for _, s := range d.Disabled {
		for _, name := range []string{s.Name, transform.AppName(s.Name)} {
			svc := p.DisabledServices[name]
			delete(p.DisabledServices, name)
			p.Services[name] = svc
		}
	}
	// A moved service may depend on a still-disabled one; drop that edge.
	for name, svc := range p.Services {
		for dep := range svc.DependsOn {
			if _, ok := p.Services[dep]; !ok {
				svc.DependsOn = maps.Clone(svc.DependsOn)
				delete(svc.DependsOn, dep)
				p.Services[name] = svc
			}
		}
	}
	return &p
}
