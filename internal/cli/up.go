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
	"github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

func upCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var pull string
	var forceUnlock, noBuild bool
	cmd := &cobra.Command{
		Use:   "up [SERVICE...]",
		Short: "Create or bounce the project",
		Long: `Create or bounce the project.

up always runs detached and always waits until every Service has converged:
there is no attached mode, so Ctrl-C stops the bounce, never the project. -d/--detach
and --wait are accepted for docker compose compatibility and change nothing.

Every service with build: is built first, as with docker compose up --build:
a changed Dockerfile or build arg makes a new image and so a bounce, while an
unchanged one is a cached no-op. --no-build skips building.`,
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
			if err := prePull(ctx, l, sel.Services, pull, true, imagePresent(dockerCli)); err != nil {
				return Exit(2, err)
			}
			return withLock(ctx, dockerCli, l, forceUnlock, func() error {
				// 0. Build under the lock, so a concurrent up cannot retag the
				// image before pinImage resolves it.
				if !noBuild {
					if err := build(ctx, l, sel, pull == types.PullPolicyAlways); err != nil {
						return Exit(2, err)
					}
				}
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
	cmd.Flags().BoolVar(&noBuild, "no-build", false, "Don't build images; services with build: need theirs already")
	cmd.Flags().Bool("build", true, "Compatibility only: up always builds services with build:")
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
//
// A replica that was created but never started is removed instead: a run
// died between creating it and running its revision's pre_start hooks, and
// starting it would skip them. The bounce creates it again, hooks first.
func startStopped(ctx context.Context, l *loaded, svc config.Service) error {
	reps, err := l.Engine.Replicas(ctx, l.Derived.Project.Name, svc.Name)
	if err != nil {
		return err
	}
	stopped := false
	for _, r := range reps {
		switch {
		case r.Running:
		case r.Started.IsZero():
			if err := l.Engine.Remove(ctx, r.ID); err != nil {
				return err
			}
		default:
			stopped = true
		}
	}
	if !stopped {
		return nil
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
	app, err := pinImage(ctx, l, l.Derived.Project.Services[transform.AppName(svc.Name)])
	if err != nil {
		return app, err
	}
	cur, err := current(ctx, l, svc.Name)
	if err != nil {
		return app, err
	}
	if cur != nil && app.GetScale() == 0 {
		l.event("Service "+svc.Name, api.Warning, "Scaling to 0 replicas:", "history lives on the replicas and is lost at 0")
	}
	if app.Build != nil && cur[labelImage] != "" && cur[labelImage] != app.Labels[labelImage] &&
		sameBuild(ctx, l, svc.Name, cur[labelImage], app.Labels[labelImage]) {
		app.Labels[labelImage] = cur[labelImage] // pinImage's copy
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
//
// skipBuilt skips every service with build: (up builds them; undo restores
// image IDs no registry serves). Otherwise, as docker compose pull, one with
// build: is pulled only when the user gave it an image:.
func prePull(ctx context.Context, l *loaded, svcs types.Services, override string, skipBuilt bool, present func(ctx context.Context, image, platform string) bool) error {
	var names []string
	images := map[string]bool{}
	for name, svc := range svcs {
		policy := override
		if policy == "" {
			policy = svc.PullPolicy
		}
		if svc.Build != nil && (skipBuilt || l.Project.Services[original(l, name)].Image == "") {
			continue // built instead; --pull always pulls its base images
		}
		switch policy {
		case types.PullPolicyNever, types.PullPolicyBuild:
			continue
		case types.PullPolicyAlways:
		default:
			if images[svc.Image+" "+svc.Platform] || present(ctx, svc.Image, svc.Platform) {
				continue
			}
		}
		names = append(names, name)
		images[svc.Image+" "+svc.Platform] = true
	}
	p := &types.Project{Name: l.Derived.Project.Name, WorkingDir: l.Derived.Project.WorkingDir, Services: types.Services{}}
	for _, n := range names {
		s := svcs[n]
		s.PullPolicy = types.PullPolicyAlways // Compose skips missing/never services otherwise
		s.DependsOn = nil                     // the dependencies are not in this project
		p.Services[n] = s
	}
	if lockImage := proxyImage(l); override != types.PullPolicyNever && !images[lockImage+" "] && !present(ctx, lockImage, "") {
		lock := types.ServiceConfig{Name: "bouncer-lock"}
		lock.Image, lock.PullPolicy = lockImage, types.PullPolicyAlways
		p.Services[lock.Name] = lock
	}
	if len(p.Services) == 0 {
		return nil
	}
	return l.Compose.Pull(ctx, p, api.PullOptions{})
}

// build builds every service of sel with build: through Compose, always (a
// cached build is quick, and an unchanged image ID means no bounce); pull
// pulls the base images too. It builds the user's services, not the derived
// ones, where a Service's name is its proxy (additional_contexts: service:S
// must mean S's app); the image, <project>-<S> or image:, is the one the
// replicas run.
func build(ctx context.Context, l *loaded, sel *types.Project, pull bool) error {
	var names []string
	for name, svc := range sel.Services {
		if svc.Build != nil {
			names = append(names, original(l, name))
		}
	}
	if len(names) == 0 {
		return nil
	}
	p, err := l.Project.WithServicesEnabled(names...) // a copy: Compose writes into it
	if err != nil {
		return err
	}
	return l.Compose.Build(ctx, p, api.BuildOptions{Services: names, Pull: pull})
}

// original is the user's service a derived one comes from: S for S-app.
func original(l *loaded, name string) string {
	for _, s := range l.Derived.Services {
		if transform.AppName(s.Name) == name {
			return s.Name
		}
	}
	return name
}

// labelImage is the exact image a replica's reference resolved to when its
// spec was made: part of the spec, so part of its hash.
const labelImage = transform.LabelPrefix + "image"

// pinImage labels a replica service with the exact image its reference names
// now, so a new image under the same reference (a rebuild, a pull of a moved
// tag) is a new spec hash, so a bounce, and undo can run that image again,
// as Compose's image-digest label marks a container outdated. The replica
// keeps the reference as written, so containers, ps and history stay
// readable. The exact image is repo@digest when the image has a digest for
// the reference's repository (undo can pull it again); else, for a built or
// locally tagged image, its image ID. A reference with a digest is exact as
// written.
func pinImage(ctx context.Context, l *loaded, app types.ServiceConfig) (types.ServiceConfig, error) {
	exact := app.Image
	if app.Build != nil || !strings.Contains(app.Image, "@") {
		id, digests, err := l.Engine.Image(ctx, app.Image)
		if err != nil {
			return app, fmt.Errorf("%s: %w", app.Name, err)
		}
		exact = id
		if d := repoDigest(app.Image, digests); app.Build == nil && d != "" {
			exact = d
		}
	}
	labels := types.Labels{}
	maps.Copy(labels, app.Labels) // never write into the shared derived project
	labels[labelImage] = exact
	app.Labels = labels
	return app, nil
}

// sameBuild: the image just built holds the platform manifest a running
// replica pinned to running runs. With BuildKit and the containerd image
// store an unchanged rebuild is a new image ID, because its provenance
// attestation changes, while its image manifest does not; keeping the
// running pin makes it no new revision, so no bounce.
func sameBuild(ctx context.Context, l *loaded, svc, running, built string) bool {
	reps, err := l.Engine.Replicas(ctx, l.Derived.Project.Name, svc)
	if err != nil {
		return false
	}
	manifests, err := l.Engine.ImageManifests(ctx, built)
	if err != nil {
		return false
	}
	for _, r := range reps {
		if r.Running && r.Labels[labelImage] == running && r.Manifest != "" {
			return slices.Contains(manifests, r.Manifest)
		}
	}
	return false
}

// repoDigest is ref's repository @ the digest under which it holds the
// image, or "" when that registry never served it (built or only tagged
// locally).
func repoDigest(ref string, repoDigests []string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	for _, rd := range repoDigests {
		if c, err := reference.ParseNormalizedNamed(rd); err == nil && c.Name() == named.Name() {
			if d, ok := c.(reference.Digested); ok {
				return reference.FamiliarName(named) + "@" + d.Digest().String()
			}
		}
	}
	return ""
}

// runImage is the image a stored spec runs: the exact one it was pinned to,
// or, for a spec stored before pinning, its reference.
func runImage(app types.ServiceConfig) string {
	if exact := app.Labels[labelImage]; exact != "" {
		return exact
	}
	return app.Image
}

// imagePresent: the image is local, for platform (os/arch[/variant]) when
// one is given: a tag can be held for another platform only, and creating
// the container would fail.
func imagePresent(dockerCli command.Cli) func(ctx context.Context, image, platform string) bool {
	return func(ctx context.Context, image, platform string) bool {
		if platform == "" {
			_, err := dockerCli.Client().ImageInspect(ctx, image)
			return err == nil
		}
		f := append(strings.Split(strings.ToLower(platform), "/"), "", "")
		want := ocispec.Platform{OS: f[0], Architecture: f[1], Variant: f[2]}
		res, err := dockerCli.Client().ImageInspect(ctx, image, client.ImageInspectWithPlatform(&want))
		if err != nil { // not held for that platform, or an engine before API 1.49
			if res, err = dockerCli.Client().ImageInspect(ctx, image); err != nil {
				return false
			}
		}
		return res.Os == want.OS && res.Architecture == want.Architecture && (want.Variant == "" || res.Variant == want.Variant)
	}
}
