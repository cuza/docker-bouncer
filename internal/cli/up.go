package cli

import (
	"context"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			l, err := load(ctx, dockerCli, pf)
			if err != nil {
				return err
			}
			switch pull {
			case "", types.PullPolicyAlways, types.PullPolicyMissing, types.PullPolicyNever:
			default:
				return Exit(2, fmt.Errorf(`--pull: %q is not "always", "missing" or "never"`, pull))
			}
			if err := prePull(ctx, l, pull, imagePresent(dockerCli)); err != nil {
				return Exit(2, err)
			}
			name := l.Derived.Project.Name
			release, err := lock.Acquire(ctx, lock.NewDocker(dockerCli.Client()), name,
				proxyImage(l), owner(), staleAfter(l), forceUnlock, time.Now())
			if err != nil {
				return Exit(1, err)
			}
			defer func() {
				if err := release(context.WithoutCancel(ctx)); err != nil {
					fmt.Fprintf(dockerCli.Err(), "warning: could not release lock %s-bouncer-lock: %v\n", name, err)
				}
			}()

			// 1. Plain services and proxies through Compose (normal convergence).
			if err := upPlainAndProxies(ctx, dockerCli, l); err != nil {
				return Exit(1, err)
			}
			// 2. Every Service bounces in parallel.
			upID := time.Now().UTC().Format("20060102T150405Z")
			var g errgroup.Group
			for _, svc := range selected(l.Derived.Services, args) {
				g.Go(func() error {
					if err := startStopped(ctx, l, svc, logf(dockerCli)); err != nil {
						return err
					}
					app, err := desiredApp(ctx, l, svc, upID)
					if err != nil {
						return err
					}
					return upService(ctx, l, svc, app, logf(dockerCli))
				})
			}
			// bounce.ErrFailed or an engine error: old replicas still serve
			return Exit(1, g.Wait())
		},
	}
	cmd.Flags().StringVar(&pull, "pull", "", `Pull policy override: "always", "missing", "never"`)
	cmd.Flags().BoolVar(&forceUnlock, "force-unlock", false, "Take over a lock left by another run")
	return cmd
}

// upPlainAndProxies converges everything except replicas the normal Compose
// way. A proxy is only recreated when its own definition changed.
func upPlainAndProxies(ctx context.Context, dockerCli command.Cli, l *loaded) error {
	p := noPull(l.Derived.Project.WithServicesDisabled(appNames(l)...))
	for _, s := range l.Derived.Services {
		running, err := l.Engine.Container(ctx, p.Name, map[string]string{transform.LabelRole: transform.RoleProxy, transform.LabelService: s.Name})
		if err != nil || running == nil {
			continue
		}
		want, err := compose.ServiceHash(p.Services[s.Name])
		if err == nil && running.Labels[api.ConfigHashLabel] != want {
			fmt.Fprintf(dockerCli.Err(), "warning: %s: proxy definition changed; recreating it interrupts traffic briefly\n", s.Name)
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
func startStopped(ctx context.Context, l *loaded, svc config.Service, log func(string, ...any)) error {
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
		log("%s: starting stopped replicas: %v (the bounce drains the dead ones)", svc.Name, err)
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
			if strings.HasPrefix(k, "bouncer.") {
				app.Labels[k] = v
			}
		}
		return app, nil
	}
	return revision.Stamp(app, cur, upID, svc.Spec.HistoryMax, time.Now())
}

func upService(ctx context.Context, l *loaded, svc config.Service, app types.ServiceConfig, log func(string, ...any)) error {
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
		Scaler: composeScaler{l}, Log: log, Now: time.Now,
	}).Run(ctx)
}

// prePull is the only pull, done before the lock; Compose gets pull_policy
// never everywhere else. Policy: --pull overrides; else the service's
// pull_policy (default missing; "daily" counts as missing). The lock
// container's image (the proxy image) is pulled when missing too.
func prePull(ctx context.Context, l *loaded, override string, present func(context.Context, string) bool) error {
	var names []string
	images := map[string]bool{}
	for name, svc := range l.Derived.Project.Services {
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
		s := l.Derived.Project.Services[n]
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
