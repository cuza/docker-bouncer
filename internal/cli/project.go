package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

type loaded struct {
	Compose api.Compose
	Project *types.Project // the user's
	Derived *transform.Result
	Engine  engine.Engine
	Events  api.EventProcessor          // the --progress display
	mu      *sync.Mutex                 // one Compose call at a time; bounce loops stay parallel
	inv     invocation                  // how this run loaded the project
	recheck func(context.Context) error // run by withLock once the lock is held
}

func load(ctx context.Context, dockerCli command.Cli, pf *ProjectFlags) (*loaded, error) {
	ev, err := newEvents(dockerCli, pf.Progress, pf.Timestamps)
	if err != nil {
		return nil, err
	}
	return loadWith(ctx, dockerCli, pf, ev)
}

// loadWith loads the project into an existing display (refresh shares one
// across projects).
func loadWith(ctx context.Context, dockerCli command.Cli, pf *ProjectFlags, ev api.EventProcessor) (*loaded, error) {
	c, err := compose.NewComposeService(dockerCli, compose.WithEventProcessor(inner{ev}))
	if err != nil {
		return nil, err
	}
	p, err := c.LoadProject(ctx, api.ProjectLoadOptions{
		ProjectName: pf.Name, ConfigPaths: pf.Files, WorkingDir: pf.Dir,
		EnvFiles: pf.EnvFiles, Profiles: pf.Profiles,
	})
	if err != nil {
		return nil, Exit(2, err)
	}
	d, err := transform.Apply(p)
	if err != nil {
		return nil, Exit(2, err)
	}
	// Compose finds containers by the labels its loader puts on each service;
	// give the derived services their own.
	for _, s := range d.Services {
		proxy, app := d.Project.Services[s.Name], d.Project.Services[transform.AppName(s.Name)]
		proxy.CustomLabels = p.Services[s.Name].CustomLabels
		app.CustomLabels = maps.Clone(proxy.CustomLabels).Add(api.ServiceLabel, app.Name)
		d.Project.Services[proxy.Name], d.Project.Services[app.Name] = proxy, app
	}
	setProject(ev, d)
	return &loaded{Compose: c, Project: p, Derived: d, Engine: engine.NewDocker(dockerCli.Client()), Events: ev, mu: &sync.Mutex{}, inv: newInvocation(p, pf)}, nil
}

func appNames(l *loaded) []string {
	var out []string
	for _, s := range l.Derived.Services {
		out = append(out, transform.AppName(s.Name))
	}
	return out
}

// expand maps each Service name to its proxy and replicas; other names pass
// through. Empty means all.
func expand(l *loaded, args []string) []string {
	var out []string
	for _, a := range args {
		out = append(out, a)
		for _, s := range l.Derived.Services {
			if s.Name == a {
				out = append(out, transform.AppName(a))
			}
		}
	}
	return out
}

type composeScaler struct{ l *loaded }

// ScaleUp creates replicas with the desired config up to total and leaves
// existing ones alone (Recreate=never); Bouncer never scales down via Compose.
func (c composeScaler) ScaleUp(ctx context.Context, app types.ServiceConfig, total int) error {
	p, err := c.l.Derived.Project.WithSelectedServices([]string{app.Name}, types.IgnoreDependencies)
	if err != nil {
		return err
	}
	if app.Deploy != nil { // shared with the derived project; SetScale writes into it
		d := *app.Deploy
		app.Deploy = &d
	}
	app.SetScale(total)
	app.DependsOn = nil                                  // its dependencies are converged and disabled here
	app.CustomLabels = p.Services[app.Name].CustomLabels // a restored spec has none
	app.PullPolicy = types.PullPolicyNever               // prePull is the only pull
	p.Services[app.Name] = app
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	return c.l.Compose.Up(ctx, p, api.UpOptions{
		Create: api.CreateOptions{Services: []string{app.Name}, Recreate: api.RecreateNever, RecreateDependencies: api.RecreateNever},
		Start:  api.StartOptions{Project: p, Services: []string{app.Name}},
	})
}

// noPull sets pull_policy never on a project copy handed to Compose: prePull
// is the only pull, done before the lock.
func noPull(p *types.Project) *types.Project {
	for n, s := range p.Services {
		s.PullPolicy = types.PullPolicyNever
		p.Services[n] = s
	}
	return p
}

func selected(svcs []config.Service, args []string) []config.Service {
	if len(args) == 0 {
		return svcs
	}
	want := map[string]bool{}
	for _, a := range args {
		want[a] = true
	}
	var out []config.Service
	for _, s := range svcs {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

func owner() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s@%s pid %d", os.Getenv("USER"), h, os.Getpid())
}

func staleAfter(l *loaded) time.Duration {
	d := 5 * time.Minute
	for _, s := range l.Derived.Services {
		if t := s.Spec.HealthTimeout * time.Duration(len(l.Derived.Services)); t > d {
			d = t
		}
	}
	return d
}

func proxyImage(l *loaded) string {
	if len(l.Derived.Services) > 0 {
		return l.Derived.Services[0].Spec.ProxyImage
	}
	return config.DefaultProxyImage
}

// show brackets a command's run in the display; call the result when done.
func (l *loaded) show(ctx context.Context, operation string) func() {
	l.Events.Start(ctx, operation)
	return func() { l.Events.Done(operation, true) }
}

// event reports one Bouncer step in the display, the way Compose reports its own.
func (l *loaded) event(id string, status api.EventStatus, text string, details ...string) {
	l.Events.On(api.Resource{ID: id, Status: status, Text: text, Details: strings.Join(details, " ")})
}
