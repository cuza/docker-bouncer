package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/bounce"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/lock"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

func upCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var pull string
	var forceUnlock bool
	cmd := &cobra.Command{
		Use:   "up [SERVICE...]",
		Short: "Create or bounce the project",
		Long: `Create or bounce the project.

up always runs detached and always waits until every Service has converged:
there is no attached mode, so Ctrl-C stops the bounce, never the project. -d/--detach
and --wait are accepted for docker compose compatibility and change nothing.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			l, err := load(ctx, dockerCli, pf)
			if err != nil {
				return err
			}
			defer l.show(ctx, "up")()
			switch pull {
			case "", types.PullPolicyAlways, types.PullPolicyMissing, types.PullPolicyNever:
			default:
				return Exit(2, fmt.Errorf(`--pull: %q is not "always", "missing" or "never"`, pull))
			}
			sel, err := upProject(l, args)
			if err != nil {
				return err
			}
			if err := prePull(ctx, l, sel.Services, pull, imagePresent(dockerCli)); err != nil {
				return Exit(2, err)
			}
			return withLock(ctx, dockerCli, l, forceUnlock, func() error {
				// 1. Plain services and proxies through Compose (normal convergence).
				if err := upPlainAndProxies(ctx, l, sel); err != nil {
					return Exit(1, err)
				}
				// 2. Every Service bounces in parallel.
				upID := time.Now().UTC().Format("20060102T150405Z")
				return bounceAll(ctx, l, selected(l.Derived.Services, args), func(svc config.Service) (types.ServiceConfig, error) {
					return desiredApp(ctx, l, svc, upID)
				})
			})
		},
	}
	cmd.Flags().StringVar(&pull, "pull", "", `Pull policy override: "always", "missing", "never"`)
	cmd.Flags().BoolVar(&forceUnlock, "force-unlock", false, "Take over a lock left by another run")
	cmd.Flags().BoolP("detach", "d", true, "Compatibility only: up always runs detached")
	cmd.Flags().Bool("wait", true, "Compatibility only: up always waits for every Service to converge")
	return cmd
}

// withLock runs fn holding the project's lock and releases it even when ctx
// was cancelled.
func withLock(ctx context.Context, dockerCli command.Cli, l *loaded, force bool, fn func() error) error {
	name := l.Derived.Project.Name
	release, err := lock.Acquire(ctx, lock.NewDocker(dockerCli.Client()), name,
		proxyImage(l), owner(), staleAfter(l), force, time.Now())
	if err != nil {
		return Exit(1, err)
	}
	id := "Lock " + name + "-bouncer-lock"
	l.event(id, api.Done, "Acquired")
	defer func() {
		if err := release(context.WithoutCancel(ctx)); err != nil {
			l.event(id, api.Warning, "Release failed:", err.Error())
		} else {
			l.event(id, api.Done, "Released")
		}
	}()
	return fn()
}

// bounceAll bounces every Service in parallel to the replica config app
// returns for it.
func bounceAll(ctx context.Context, l *loaded, svcs []config.Service, app func(config.Service) (types.ServiceConfig, error)) error {
	var g errgroup.Group
	for _, svc := range svcs {
		g.Go(func() error {
			err := startStopped(ctx, l, svc)
			if err == nil {
				var a types.ServiceConfig
				if a, err = app(svc); err == nil {
					err = upService(ctx, l, svc, a)
				}
			}
			if err != nil && !errors.Is(err, bounce.ErrFailed) { // the runner reported that one
				l.event("Service "+svc.Name, api.Error, "Failed: "+err.Error())
			}
			return err
		})
	}
	// bounce.ErrFailed or an engine error: old replicas still serve
	return Exit(1, g.Wait())
}

// upProject is the part of the derived project `up [SERVICE...]` converges:
// the named services, their replicas and dependencies; all without args.
func upProject(l *loaded, args []string) (*types.Project, error) {
	for _, a := range args {
		if _, ok := l.Project.Services[a]; !ok {
			return nil, configErr("%s is not a service", a)
		}
	}
	p, err := l.Derived.Project.WithSelectedServices(expand(l, args))
	if err != nil {
		return nil, configErr("%v", err)
	}
	return p, nil
}

// upPlainAndProxies converges sel except replicas the normal Compose way. A
// proxy is only recreated when its own definition changed.
func upPlainAndProxies(ctx context.Context, l *loaded, sel *types.Project) error {
	p := noPull(sel.WithServicesDisabled(appNames(l)...))
	for _, s := range l.Derived.Services {
		if _, ok := p.Services[s.Name]; !ok {
			continue
		}
		running, err := l.Engine.Container(ctx, p.Name, map[string]string{transform.LabelRole: transform.RoleProxy, transform.LabelService: s.Name})
		if err != nil || running == nil {
			continue
		}
		want, err := compose.ServiceHash(p.Services[s.Name])
		if err == nil && running.Labels[api.ConfigHashLabel] != want {
			l.event("Service "+s.Name, api.Warning, "Proxy definition changed: recreating", "(interrupts traffic briefly)")
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Compose.Up(ctx, p, api.UpOptions{
		Create: api.CreateOptions{Recreate: api.RecreateDiverged, RecreateDependencies: api.RecreateDiverged},
		Start:  api.StartOptions{Project: p},
	})
}

// startStopped starts the Service's stopped replicas (after `stop`, or a host
// restart) so they count as running: the planner drains old ones and keeps
// current ones instead of waiting on them until bounce_health_timeout.
func startStopped(ctx context.Context, l *loaded, svc config.Service) error {
	reps, err := l.Engine.Replicas(ctx, l.Derived.Project.Name, svc.Name)
	if err != nil || !slices.ContainsFunc(reps, func(r engine.Replica) bool { return !r.Running }) {
		return err
	}
	p, err := l.Derived.Project.WithSelectedServices([]string{transform.AppName(svc.Name)}, types.IgnoreDependencies)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.Compose.Start(ctx, p.Name, api.StartOptions{Project: noPull(p)}); err != nil {
		l.event("Service "+svc.Name, api.Warning, "Starting stopped replicas failed:", err.Error(), "(the bounce drains the dead ones)")
	}
	return nil
}

// current returns the labels of a running replica of the newest revision.
func current(ctx context.Context, l *loaded, svc string) (map[string]string, error) {
	reps, err := l.Engine.Replicas(ctx, l.Derived.Project.Name, svc)
	if err != nil {
		return nil, err
	}
	var best map[string]string
	bestRev := -1
	for _, r := range reps {
		rev, _ := strconv.Atoi(r.Labels[revision.LabelRevision])
		if r.Running && rev > bestRev {
			best, bestRev = r.Labels, rev
		}
	}
	return best, nil
}

// desiredApp: the file's replica config, stamped as a new revision only when
// it differs from what runs.
func desiredApp(ctx context.Context, l *loaded, svc config.Service, upID string) (types.ServiceConfig, error) {
	app := l.Derived.Project.Services[transform.AppName(svc.Name)]
	cur, err := current(ctx, l, svc.Name)
	if err != nil {
		return app, err
	}
	_, hash, err := revision.Encode(app)
	if err != nil {
		return app, err
	}
	app.Labels = maps.Clone(app.Labels) // never write into the shared derived project
	if cur != nil && cur[revision.LabelSpecHash] == hash {
		for k, v := range cur {
			if strings.HasPrefix(k, transform.LabelPrefix) {
				app.Labels[k] = v
			}
		}
		return app, nil
	}
	app, kept, err := revision.Stamp(app, cur, upID, svc.Spec.HistoryMax, time.Now())
	warnTrimmed(l, svc, kept)
	return app, err
}

func warnTrimmed(l *loaded, svc config.Service, kept int) {
	if kept > 0 {
		l.event("Service "+svc.Name, api.Warning, "History trimmed:", fmt.Sprintf("to %d revisions (%d KiB label budget)", kept, revision.HistoryBudget>>10))
	}
}

func upService(ctx context.Context, l *loaded, svc config.Service, app types.ServiceConfig) error {
	proxy, err := l.Engine.Container(ctx, l.Derived.Project.Name, map[string]string{transform.LabelRole: transform.RoleProxy, transform.LabelService: svc.Name})
	if err != nil {
		return err
	}
	if proxy == nil {
		return fmt.Errorf("%s: proxy container not found", svc.Name)
	}
	px := engine.NewProxy(l.Engine, proxy.ID, svc)
	// A just-(re)created proxy needs a moment before its admin port answers.
	for deadline := time.Now().Add(time.Minute); ; {
		_, err := px.Health(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: proxy not ready: %w", svc.Name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return (&bounce.Runner{
		Project: l.Derived.Project.Name, Svc: svc, App: app, N: app.GetScale(),
		Engine: l.Engine, Proxy: px,
		Scaler: composeScaler{l}, Events: l.Events, Now: time.Now,
	}).Run(ctx)
}

// prePull is the only pull, done before the lock; Compose gets pull_policy
// never everywhere else. Policy: --pull overrides; else the service's
// pull_policy (default missing; "daily" counts as missing). The lock
// container's image (the proxy image) is pulled when missing too.
func prePull(ctx context.Context, l *loaded, svcs types.Services, override string, present func(context.Context, string) bool) error {
	var names []string
	images := map[string]bool{}
	for name, svc := range svcs {
		policy := override
		if policy == "" {
			policy = svc.PullPolicy
		}
		switch policy {
		case types.PullPolicyNever, types.PullPolicyBuild:
			continue
		case types.PullPolicyAlways:
		default:
			if images[svc.Image] || present(ctx, svc.Image) {
				continue
			}
		}
		names = append(names, name)
		images[svc.Image] = true
	}
	p := &types.Project{Name: l.Derived.Project.Name, WorkingDir: l.Derived.Project.WorkingDir, Services: types.Services{}}
	for _, n := range names {
		s := svcs[n]
		s.PullPolicy = types.PullPolicyAlways // Compose skips missing/never services otherwise
		p.Services[n] = s
	}
	if lockImage := proxyImage(l); override != types.PullPolicyNever && !images[lockImage] && !present(ctx, lockImage) {
		lock := types.ServiceConfig{Name: "bouncer-lock"}
		lock.Image, lock.PullPolicy = lockImage, types.PullPolicyAlways
		p.Services[lock.Name] = lock
	}
	if len(p.Services) == 0 {
		return nil
	}
	return l.Compose.Pull(ctx, p, api.PullOptions{})
}

func imagePresent(dockerCli command.Cli) func(context.Context, string) bool {
	return func(ctx context.Context, image string) bool {
		_, err := dockerCli.Client().ImageInspect(ctx, image)
		return err == nil
	}
}
