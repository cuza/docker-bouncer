// Package transform derives the project Compose actually runs: each
// x-bouncer service S becomes an Envoy proxy named S plus replicas S-app.
package transform

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/envoy"
)

const (
	LabelManaged   = "bouncer.managed"
	LabelRole      = "bouncer.role"
	LabelService   = "bouncer.service"
	LabelAdminPort = "bouncer.admin-port"

	RoleProxy   = "proxy"
	RoleReplica = "replica"
	RoleLock    = "lock"
)

type Result struct {
	Project  *types.Project
	Services []config.Service
}

func AppName(service string) string { return service + "-app" }

func Apply(in *types.Project) (*Result, error) {
	p := *in
	p.Services = types.Services{}
	for k, v := range in.Services {
		p.Services[k] = v
	}
	var out []config.Service
	for _, name := range sortedKeys(in.Services) {
		svc := in.Services[name]
		bs, err := config.Parse(svc)
		if err != nil {
			return nil, err
		}
		if bs == nil {
			continue
		}
		app := AppName(name)
		if _, taken := in.Services[app]; taken {
			return nil, &config.Error{Service: name, Problems: []string{fmt.Sprintf("service %q already exists; it is reserved for the replicas", app)}}
		}
		p.Services[name] = proxy(svc, *bs)
		p.Services[app] = replicas(svc)
		out = append(out, *bs)
	}
	return &Result{Project: &p, Services: out}, nil
}

func proxy(svc types.ServiceConfig, bs config.Service) types.ServiceConfig {
	bootstrap := envoy.Bootstrap(bs)
	seed := envoy.Clusters(bs, []string{envoy.SeedHost(bs)})
	interval := types.Duration(5 * time.Second)
	adminPort := strconv.Itoa(bs.AdminPort)
	p := types.ServiceConfig{
		Name:     svc.Name,
		Restart:  types.RestartPolicyUnlessStopped,
		Profiles: svc.Profiles,
	}
	// Image, Ports, etc. are promoted from embedded structs; assign them here.
	p.Image = bs.Spec.ProxyImage
	p.Entrypoint = types.ShellCommand(envoy.Entrypoint())
	p.Environment = types.MappingWithEquals{"BOUNCER_BOOTSTRAP": &bootstrap, "BOUNCER_CDS": &seed}
	p.Ports = svc.Ports
	p.Expose = svc.Expose
	p.Networks = svc.Networks
	p.Logging = svc.Logging
	p.PullPolicy = svc.PullPolicy
	p.Labels = types.Labels{
		LabelManaged: "true", LabelRole: RoleProxy, LabelService: svc.Name, LabelAdminPort: adminPort,
	}
	p.HealthCheck = &types.HealthCheckConfig{
		Test: types.HealthCheckTest{"CMD", "bash", "-c",
			`exec 3<>/dev/tcp/127.0.0.1/` + adminPort + ` && printf 'GET /ready HTTP/1.0\r\n\r\n' >&3 && grep -q LIVE <&3`},
		Interval: &interval,
	}
	return p
}

func replicas(svc types.ServiceConfig) types.ServiceConfig {
	app := svc
	app.Name = AppName(svc.Name)
	app.Ports = nil
	app.Extensions = nil
	if svc.Networks != nil {
		app.Networks = map[string]*types.ServiceNetworkConfig{}
		for n, cfg := range svc.Networks {
			if cfg == nil {
				app.Networks[n] = nil
				continue
			}
			c := *cfg
			c.Aliases = nil
			app.Networks[n] = &c
		}
	}
	app.Labels = types.Labels{}
	for k, v := range svc.Labels {
		app.Labels[k] = v
	}
	app.Labels[LabelManaged] = "true"
	app.Labels[LabelRole] = RoleReplica
	app.Labels[LabelService] = svc.Name
	return app
}

func sortedKeys(s types.Services) []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
