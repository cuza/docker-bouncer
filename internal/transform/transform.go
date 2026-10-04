// Package transform derives the project Compose actually runs: each
// x-bouncer service S becomes an Envoy proxy named S plus replicas S-app.
package transform

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/envoy"
)

const (
	// LabelPrefix starts every label bouncer sets (reverse DNS, like com.docker.compose.*).
	LabelPrefix    = "dev.cuza.bouncer."
	LabelManaged   = LabelPrefix + "managed"
	LabelRole      = LabelPrefix + "role"
	LabelService   = LabelPrefix + "service"
	LabelAdminPort = LabelPrefix + "admin-port"

	RoleProxy   = "proxy"
	RoleReplica = "replica"
	RoleLock    = "lock"
)

// Result is the derived project. Services are the active x-bouncer services;
// Disabled are those whose profile is inactive, derived into
// Project.DisabledServices so down and ps see their proxy and replicas.
type Result struct {
	Project  *types.Project
	Services []config.Service
	Disabled []config.Service
}

func AppName(service string) string { return service + "-app" }

// MaxDNSName is the longest DNS label; Envoy finds replicas by container name.
const MaxDNSName = 63

func Apply(in *types.Project) (*Result, error) {
	if err := validate(in); err != nil {
		return nil, err
	}
	p := *in
	p.Services, p.DisabledServices = types.Services{}, types.Services{}
	maps.Copy(p.Services, in.Services)
	maps.Copy(p.DisabledServices, in.DisabledServices)
	derive := func(from, into types.Services) ([]config.Service, error) {
		var out []config.Service
		for _, name := range sortedKeys(from) {
			svc := from[name]
			bs, err := config.Parse(svc)
			if err != nil {
				return nil, err
			}
			if bs == nil {
				continue
			}
			app := AppName(name)
			_, taken := in.Services[app]
			_, takenDisabled := in.DisabledServices[app]
			if taken || takenDisabled {
				return nil, &config.Error{Service: name, Problems: []string{fmt.Sprintf("service %q already exists; it is reserved for the replicas", app)}}
			}
			into[name] = proxy(svc, *bs)
			into[app] = replicas(in.Name, svc)
			out = append(out, *bs)
		}
		return out, nil
	}
	out, err := derive(in.Services, p.Services)
	if err != nil {
		return nil, err
	}
	disabled, err := derive(in.DisabledServices, p.DisabledServices)
	if err != nil {
		return nil, err
	}
	// A dependent of Service S also depends on S-app, so stop and down stop it
	// before S's replicas, not only before S's proxy.
	for _, svcs := range []types.Services{p.Services, p.DisabledServices} {
		for name, svc := range svcs {
			var deps types.DependsOnConfig
			for _, bs := range append(out, disabled...) {
				if d, ok := svc.DependsOn[bs.Name]; ok {
					if deps == nil {
						deps = maps.Clone(svc.DependsOn) // shared with in's service
					}
					deps[AppName(bs.Name)] = d
				}
			}
			if deps != nil {
				svc.DependsOn = deps
				svcs[name] = svc
			}
		}
	}
	return &Result{Project: &p, Services: out, Disabled: disabled}, nil
}

// validate rejects what a proxy plus replicas cannot honour, across the whole
// project (active profile or not): host, none or another container's or
// service's networking and link-local addresses on a Service, a namespace or volumes
// shared with a Service (they would reach its proxy), replica names longer
// than a DNS label, and user labels with Bouncer's prefix on any service
// (Bouncer would take its containers for proxies or replicas).
func validate(in *types.Project) error {
	all := maps.Clone(in.DisabledServices)
	if all == nil {
		all = types.Services{}
	}
	maps.Copy(all, in.Services)
	bouncer := map[string]bool{}
	for name, svc := range all {
		_, bouncer[name] = svc.Extensions[config.Extension]
	}
	for _, name := range sortedKeys(all) {
		svc := all[name]
		var problems []string
		if bouncer[name] {
			if m := svc.NetworkMode; m == "host" || m == "none" || strings.HasPrefix(m, "container:") || strings.HasPrefix(m, "service:") {
				problems = append(problems, fmt.Sprintf("network_mode %s cannot be set on a bouncer service (its proxy reaches the replicas over a network)", svc.NetworkMode))
			}
			for _, n := range sortedKeys(svc.Networks) {
				if c := svc.Networks[n]; c != nil && len(c.LinkLocalIPs) > 0 {
					problems = append(problems, fmt.Sprintf("networks.%s.link_local_ips cannot be set on a bouncer service (every replica and the proxy would claim the same addresses)", n))
				}
			}
			// Compose names replicas <project>-<service>-app-<n>; allow 4 digits.
			if long := in.Name + "-" + AppName(name) + "-1000"; len(long) > MaxDNSName {
				problems = append(problems, fmt.Sprintf("replica names like %s are %d characters, over the %d-character DNS name limit; use a shorter project name (-p) or service name", long, len(long), MaxDNSName))
			}
		}
		for _, k := range sortedKeys(svc.Labels) {
			if strings.HasPrefix(k, LabelPrefix) {
				problems = append(problems, fmt.Sprintf("label %s uses the reserved %s prefix (Bouncer finds its proxies and replicas by it)", k, LabelPrefix))
			}
		}
		refs := [][2]string{{"network_mode", svc.NetworkMode}, {"ipc", svc.Ipc}, {"pid", svc.Pid}}
		for _, v := range svc.VolumesFrom {
			refs = append(refs, [2]string{"volumes_from", v})
		}
		for _, r := range refs {
			target, isService := strings.CutPrefix(r[1], "service:")
			if !isService && r[0] == "volumes_from" && !strings.HasPrefix(r[1], "container:") {
				target, isService = r[1], true // volumes_from: [web] or [web:ro]
			}
			target, _, _ = strings.Cut(target, ":")
			if isService && bouncer[target] {
				problems = append(problems, fmt.Sprintf("%s %s points at %s's proxy; bouncer services can't share namespaces or volumes", r[0], r[1], target))
			}
		}
		if len(problems) > 0 {
			return &config.Error{Service: name, Problems: problems}
		}
	}
	return nil
}

func proxy(svc types.ServiceConfig, bs config.Service) types.ServiceConfig {
	bootstrap := envoy.Bootstrap(bs)
	seed := envoy.SeedClusters(bs)
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
			`exec 3<>/dev/tcp/127.0.0.1/` + adminPort + ` && printf 'GET /ready HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' >&3 && grep -q LIVE <&3`},
		Interval: &interval,
	}
	return p
}

func replicas(project string, svc types.ServiceConfig) types.ServiceConfig {
	app := svc
	app.Name = AppName(svc.Name)
	// Compose names a built image <project>-<service>; keep the user's
	// service name, not the replicas'.
	if app.Image == "" && app.Build != nil {
		app.Image = project + "-" + svc.Name
	}
	app.Ports = nil
	app.Extensions = nil
	// The aliases move to the proxy; pre_start hooks inherited them too.
	app.Networks = withoutAliases(svc.Networks)
	if svc.PreStart != nil {
		app.PreStart = slices.Clone(svc.PreStart)
		for i := range app.PreStart {
			app.PreStart[i].Networks = withoutAliases(app.PreStart[i].Networks)
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

func withoutAliases(nets map[string]*types.ServiceNetworkConfig) map[string]*types.ServiceNetworkConfig {
	if nets == nil {
		return nil
	}
	out := map[string]*types.ServiceNetworkConfig{}
	for n, cfg := range nets {
		if cfg != nil {
			c := *cfg
			c.Aliases = nil
			cfg = &c
		}
		out[n] = cfg
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
