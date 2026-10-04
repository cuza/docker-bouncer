package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/revision"
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
	Events  api.EventProcessor // the --progress display
	mu      *sync.Mutex        // one Compose call at a time; bounce loops stay parallel
}

func load(ctx context.Context, dockerCli command.Cli, pf *ProjectFlags) (*loaded, error) {
	ev, err := newEvents(dockerCli, pf.Progress, pf.Timestamps)
	if err != nil {
		return nil, err
	}
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
	return &loaded{Compose: c, Project: p, Derived: d, Engine: engine.NewDocker(dockerCli.Client()), Events: ev, mu: &sync.Mutex{}}, nil
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
	// Its dependencies are converged and disabled here, except providers:
	// Compose injects a provider's environment only into the dependents in
	// the project it runs the provider for, so creating replicas runs them
	// (once more, as every compose up does). Compose works on a deep copy
	// of the project, so the injected values cannot be kept for the next
	// step: each step that creates replicas runs the providers.
	names, deps := []string{app.Name}, types.DependsOnConfig{}
	for d, cfg := range app.DependsOn {
		if s, ok := c.l.Derived.Project.Services[d]; ok && s.Provider != nil {
			names, deps[d] = append(names, d), cfg
		}
	}
	p, err := c.l.Derived.Project.WithSelectedServices(names, types.IgnoreDependencies)
	if err != nil {
		return err
	}
	for _, d := range names[1:] {
		s := p.Services[d]
		s.DependsOn = nil
		p.Services[d] = s
	}
	if app.Deploy != nil { // shared with the derived project; SetScale writes into it
		d := *app.Deploy
		app.Deploy = &d
	}
	app.SetScale(total)
	app.DependsOn = nil
	if len(deps) > 0 {
		app.DependsOn = deps
		app.Environment = maps.Clone(app.Environment) // Compose writes the providers' values into (a copy of) it
		if app.Environment == nil {
			app.Environment = types.MappingWithEquals{}
		}
	}
	app.CustomLabels = p.Services[app.Name].CustomLabels // a restored spec has none
	app.PullPolicy = types.PullPolicyNever               // prePull is the only pull
	p.Services[app.Name] = app
	// The same project without the providers, for the calls after the
	// replicas' creation: hooks and starting need no provider run.
	bare := *p
	noDeps := app
	noDeps.DependsOn = nil
	bare.Services = types.Services{app.Name: noDeps}
	create := api.CreateOptions{Services: []string{app.Name}, Recreate: api.RecreateNever, RecreateDependencies: api.RecreateNever}
	start := api.StartOptions{Project: &bare, Services: []string{app.Name}}
	hooks, err := c.runsPreStart(ctx, app)
	if err != nil {
		return err
	}
	if !hooks {
		err = c.compose(func() error {
			return c.l.Compose.Up(ctx, p, api.UpOptions{Create: create, Start: api.StartOptions{Project: p, Services: start.Services}})
		})
	} else if err = c.compose(func() error { return c.l.Compose.Create(ctx, p, create) }); err == nil {
		if err = c.preStart(ctx, &bare, noDeps); err != nil {
			c.removeCreated(ctx, app)
			return err
		}
		err = c.compose(func() error { return c.l.Compose.Start(ctx, bare.Name, start) })
	}
	return err
}

// compose makes one Compose call; a hook runs between calls, so other
// Services' bounces are not held up by it.
func (c composeScaler) compose(call func() error) error {
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	return call()
}

// runsPreStart: Compose runs pre_start hooks only when no replica of the
// service runs, as on a recreate; a bounce keeps the old replicas running,
// so Bouncer runs the hooks itself before the first replica of a new
// revision starts. With none running, Compose runs them.
func (c composeScaler) runsPreStart(ctx context.Context, app types.ServiceConfig) (bool, error) {
	if len(app.PreStart) == 0 {
		return false, nil
	}
	reps, err := c.l.Engine.Replicas(ctx, c.l.Derived.Project.Name, original(c.l, app.Name))
	if err != nil {
		return false, err
	}
	running := false
	for _, r := range reps {
		if r.Labels[revision.LabelSpecHash] == app.Labels[revision.LabelSpecHash] {
			return false, nil // this revision's hooks already ran
		}
		running = running || r.Running
	}
	return running, nil
}

// preStart runs app's pre_start hooks in order, as Compose does: each hook
// container is the hook's spec (inherited from the service by compose-go)
// created through Compose, sharing the volumes of the first new replica and
// labelled as Compose's own hook containers. A hook that exits non-zero
// stops the bounce with its exit code and output.
func (c composeScaler) preStart(ctx context.Context, p *types.Project, app types.ServiceConfig) error {
	reps, err := c.l.Engine.Replicas(ctx, p.Name, original(c.l, app.Name))
	if err != nil {
		return err
	}
	// The lowest-numbered new replica, as Compose picks.
	var target *engine.Replica
	num := func(r *engine.Replica) int { n, _ := strconv.Atoi(r.Labels[api.ContainerNumberLabel]); return n }
	for i, r := range reps {
		if r.Labels[revision.LabelSpecHash] == app.Labels[revision.LabelSpecHash] && !r.Running &&
			(target == nil || num(&reps[i]) < num(target)) {
			target = &reps[i]
		}
	}
	if target == nil {
		return fmt.Errorf("%s: no new replica for the pre_start hooks", app.Name)
	}
	for i := range app.PreStart {
		if err := c.runHook(ctx, p, app, i, target.ID); err != nil {
			return fmt.Errorf("%s pre_start[%d]: %w", app.Name, i, err)
		}
	}
	return nil
}

// runHook creates, runs and always removes one hook container (keeping the
// volumes it shares with the replica), even when ctx is cancelled.
func (c composeScaler) runHook(ctx context.Context, p *types.Project, app types.ServiceConfig, i int, target string) error {
	cleanup := context.WithoutCancel(ctx)
	labels := map[string]string{api.ServiceLabel: app.Name, api.HookLabel: "pre_start", api.HookIndexLabel: strconv.Itoa(i)}
	if old, err := c.l.Engine.Container(cleanup, p.Name, labels); err != nil {
		return err
	} else if old != nil { // left by a killed run
		if err := c.l.Engine.RemoveKeepVolumes(cleanup, old.ID); err != nil {
			return err
		}
	}
	h := types.ServiceConfig{Name: fmt.Sprintf("%s-pre_start-%d", app.Name, i), ContainerSpec: app.PreStart[i].ContainerSpec}
	h.ContainerName = p.Name + api.Separator + h.Name // Compose's name for it
	if h.Image == "" {
		h.Image = app.Image
	}
	h.PullPolicy = types.PullPolicyNever
	h.VolumesFrom = append(slices.Clone(h.VolumesFrom), "container:"+target)
	h.CustomLabels = types.Labels{}
	maps.Copy(h.CustomLabels, p.Services[app.Name].CustomLabels)
	maps.Copy(h.CustomLabels, labels)
	// Compose creates it: its own create path, as for its hook containers.
	// It carries the replicas' service label, so Compose would not start
	// it as h; Bouncer does.
	hp := *p
	hp.Services = maps.Clone(p.Services)
	hp.Services[h.Name] = h
	created := c.compose(func() error {
		return c.l.Compose.Create(ctx, &hp, api.CreateOptions{Services: []string{h.Name}, Recreate: api.RecreateNever, RecreateDependencies: api.RecreateNever})
	})
	// Look it up even when Create failed: a cancelled Create may have
	// created it already.
	run, err := c.l.Engine.Container(cleanup, p.Name, labels)
	if run != nil {
		defer c.l.Engine.RemoveKeepVolumes(cleanup, run.ID)
	}
	if created != nil {
		return created
	}
	if err == nil && run == nil {
		err = fmt.Errorf("its container was not created")
	}
	if err != nil {
		return err
	}
	if err := c.l.Engine.Start(ctx, run.ID); err != nil {
		return err
	}
	for {
		cur, err := c.l.Engine.Container(ctx, p.Name, labels)
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("its container is gone")
		}
		if !cur.Running {
			if cur.ExitCode != 0 {
				out, _ := c.l.Engine.Logs(cleanup, cur.ID, 20)
				return fmt.Errorf("exited with %d:\n%s", cur.ExitCode, strings.TrimRight(out, "\n"))
			}
			return nil
		}
		c.l.Engine.Wait(ctx, p.Name, time.Second)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// removeCreated removes the replicas of app's revision that never started.
func (c composeScaler) removeCreated(ctx context.Context, app types.ServiceConfig) {
	reps, _ := c.l.Engine.Replicas(context.WithoutCancel(ctx), c.l.Derived.Project.Name, original(c.l, app.Name))
	for _, r := range reps {
		if r.Labels[revision.LabelSpecHash] == app.Labels[revision.LabelSpecHash] && !r.Running {
			c.l.Engine.Remove(context.WithoutCancel(ctx), r.ID)
		}
	}
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
