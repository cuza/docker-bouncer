package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/lock"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/distribution/reference"
	dockercli "github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type refreshOptions struct {
	all, dryRun    bool
	pull           string
	progress       string
	timestamps     bool
	ignoreFormat   bool
	present        func(context.Context, string) bool
	registryDigest func(ctx context.Context, ref string) (string, error)
}

func refreshCmd(dockerCli command.Cli, pf *ProjectFlags) *cobra.Command {
	var o refreshOptions
	cmd := &cobra.Command{
		Use:   "refresh [PROJECT...]",
		Short: "Pull moved tags and bounce each project the way its last up did",
		Long: `Pull moved tags and bounce each project the way its last up did.

Every project on this host with Bouncer replicas, or the named ones, is loaded
with the compose files, project directory, env files and profiles its last up
recorded, from any directory. Images whose tag moved in the registry are
pulled (digest references and build: services are left alone), then the
project's Services go through up: an unchanged image is a no-op, a moved one
a health-gated bounce. A Service whose last revision came from undo is held
there until the next up.

Without -a/--all, plain services and proxies are never touched (no create,
recreate or start). -a converges them as docker compose up does (a changed
image recreates the container). A project
whose lock is held is skipped. Exit 0 when every project was refreshed, up
to date or skipped; 1 when one failed; 2 on a usage or config error.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.pull != "" && o.pull != types.PullPolicyNever {
				return configErr(`--pull: %q is not "never"`, o.pull)
			}
			if len(pf.Files)+len(pf.EnvFiles)+len(pf.Profiles) > 0 || pf.Dir != "" {
				return configErr("refresh loads each project as its last up did: -f, --project-directory, --env-file and --profile don't apply")
			}
			if pf.Name != "" {
				args = append(args, pf.Name)
			}
			ev, err := newEvents(dockerCli, pf.Progress, pf.Timestamps)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			found, err := bouncerProjects(ctx, dockerCli.Client())
			if err != nil {
				return Exit(1, err)
			}
			names, err := pickProjects(found, args)
			if err != nil {
				return err
			}
			o.progress, o.timestamps, o.ignoreFormat, o.present = pf.Progress, pf.Timestamps, pf.IgnoreFormat, imagePresent(dockerCli)
			o.registryDigest = func(ctx context.Context, ref string) (string, error) {
				auth, _ := command.RetrieveAuthTokenFromImage(dockerCli.ConfigFile(), ref)
				res, err := dockerCli.Client().DistributionInspect(ctx, ref, client.DistributionInspectOptions{EncodedRegistryAuth: auth})
				return res.Descriptor.Digest.String(), err
			}
			ev.Start(ctx, "refresh")
			defer ev.Done("refresh", true)
			var res []refreshResult
			for _, name := range names {
				forProject(ev, name)
				res = append(res, refreshProject(ctx, dockerCli, ev, name, found[name], o))
			}
			forProject(ev, "")
			text, err := refreshSummary("refresh", res)
			status := api.Done
			if err != nil {
				status = api.Error
			}
			ev.On(api.Resource{ID: "Refresh", Status: status, Text: text})
			return err
		},
	}
	cmd.Flags().BoolVarP(&o.all, "all", "a", false, "Also converge plain services as docker compose up does (a changed image recreates them)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "Report what would change (with -a, plain services and proxies too); pull, create and bounce nothing")
	cmd.Flags().StringVar(&o.pull, "pull", "", `"never": use the local images, don't ask the registry`)
	return cmd
}

// hostProject is a project's replicas as found on the host.
type hostProject struct {
	labels  map[string]string // of the newest replica of the newest up-id
	created int64
	running bool
}

// bouncerProjects finds every project with Bouncer replicas, as ls does.
func bouncerProjects(ctx context.Context, c client.APIClient) (map[string]hostProject, error) {
	res, err := c.ContainerList(ctx, client.ContainerListOptions{All: true,
		Filters: client.Filters{}.Add("label", transform.LabelRole+"="+transform.RoleReplica)})
	if err != nil {
		return nil, err
	}
	out := map[string]hostProject{}
	for _, ct := range res.Items {
		name := ct.Labels[api.ProjectLabel]
		hp, seen := out[name]
		up, best := ct.Labels[revision.LabelUpID], hp.labels[revision.LabelUpID] // UTC timestamps
		// The newest container: an up that scaled replicas without a new
		// revision recorded its own invocation on them.
		if !seen || up > best || up == best && ct.Created > hp.created {
			hp.labels, hp.created = ct.Labels, ct.Created
		}
		hp.running = hp.running || string(ct.State) == "running"
		out[name] = hp
	}
	return out, nil
}

// pickProjects is every found project, sorted, or the named ones.
func pickProjects(found map[string]hostProject, args []string) ([]string, error) {
	if len(args) == 0 {
		return slices.Sorted(maps.Keys(found)), nil
	}
	var out []string
	for _, a := range args {
		if _, ok := found[a]; !ok {
			return nil, configErr("%s: no project with Bouncer replicas on this host", a)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

const (
	refreshed = "refreshed"
	wouldDo   = "would change"
	upToDate  = "up to date"
	skipped   = "skipped"
	failed    = "failed"
)

type refreshResult struct {
	status string
	err    error // with failed
}

// refreshSummary is cmd's (refresh or migrate) closing line and the exit: 0 unless a project
// failed, then the highest code among the failures (1 bounce, 2 config).
func refreshSummary(cmd string, res []refreshResult) (string, error) {
	count := map[string]int{}
	code := 0
	for _, r := range res {
		count[r.status]++
		if r.err != nil {
			c := 1
			if se, ok := errors.AsType[dockercli.StatusError](r.err); ok && se.StatusCode > c {
				c = se.StatusCode
			}
			code = max(code, c)
		}
	}
	var parts []string
	for _, s := range []string{refreshed, wouldDo, upToDate, skipped, failed} {
		if count[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count[s], s))
		}
	}
	noun := "projects"
	if len(res) == 1 {
		noun = "project"
	}
	text := fmt.Sprintf("%d %s", len(res), noun)
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, ", ")
	}
	if code == 0 {
		return text, nil
	}
	return text, Exit(code, fmt.Errorf("%s: %d of %d %s failed", cmd, count[failed], len(res), noun))
}

func refreshProject(ctx context.Context, dockerCli command.Cli, ev api.EventProcessor, name string, hp hostProject, o refreshOptions) refreshResult {
	say := func(st api.EventStatus, text string, details ...string) {
		ev.On(api.Resource{ID: "Project " + name, Status: st, Text: text, Details: strings.Join(details, " ")})
	}
	skip := func(why string) refreshResult {
		say(api.Warning, "Skipped:", why)
		return refreshResult{status: skipped}
	}
	fail := func(err error) refreshResult {
		say(api.Error, "Failed: "+err.Error())
		return refreshResult{status: failed, err: err}
	}
	if hp.labels[transform.LabelInvocation] == "" {
		return skip("no invocation recorded; run `docker bouncer up` once")
	}
	if !hp.running {
		return skip("stopped; `docker bouncer up` starts it")
	}
	labels, err := stackLabels(ctx, dockerCli.Client(), name)
	if err != nil {
		return fail(Exit(1, err))
	}
	refuse, notes := formatCheck(name, labels, true, o.ignoreFormat)
	if refuse != nil {
		return fail(Exit(1, refuse)) // a failed project, not a usage error
	}
	ev.On(notes...)
	inv, _, err := decodeInvocation(hp.labels)
	if err != nil {
		return fail(Exit(1, fmt.Errorf("invocation label: %w", err)))
	}
	l, err := loadWith(ctx, dockerCli, inv.flags(o.progress, o.timestamps), ev)
	if err != nil {
		return fail(err)
	}
	if got := l.Derived.Project.Name; got != name { // never lock, pull or bounce another project
		return fail(Exit(1, fmt.Errorf("the recorded invocation loads project %s, not %s; run docker bouncer up for %s once", got, name, name)))
	}
	if vars := shellVars(inv); len(vars) > 0 {
		say(api.Warning, "Not set in the env files: "+strings.Join(vars, ", ")+";",
			"values from the shell at the last up are not replayed; put them in an env file")
	}
	var svcs []config.Service // all but those undo holds
	for _, svc := range l.Derived.Services {
		cur, err := current(ctx, l, svc.Name)
		if err != nil {
			return fail(Exit(1, err))
		}
		if h, ok, _ := decodeInvocation(cur); ok && h.Command == "undo" {
			l.event("Service "+svc.Name, api.Warning, fmt.Sprintf("Held at revision %d by undo;", h.Revision), "run docker bouncer up to resume")
			continue
		}
		svcs = append(svcs, svc)
	}
	if o.dryRun {
		if refreshDryRun(ctx, l, svcs, o) {
			say(api.Warning, "Would change")
			return refreshResult{status: wouldDo}
		}
		say(api.Done, "Up to date")
		return refreshResult{status: upToDate}
	}
	if owner, held := lockHeld(ctx, dockerCli, l); held { // before pulling; withLock checks again
		return skip("bounce in progress by " + owner)
	}
	targets := types.Services{}
	for _, svc := range svcs {
		targets[transform.AppName(svc.Name)] = l.Derived.Project.Services[transform.AppName(svc.Name)]
	}
	if o.all {
		for _, n := range plainServices(l) {
			targets[n] = l.Derived.Project.Services[n]
		}
	}
	if len(targets) == 0 {
		say(api.Done, "Up to date")
		return refreshResult{status: upToDate}
	}
	if o.pull != types.PullPolicyNever {
		moved, err := refreshPull(ctx, l, targets, o.present)
		if err != nil {
			return fail(Exit(1, err))
		}
		if len(moved) > 0 {
			say(api.Done, "Pulled", strings.Join(moved, ", "))
		}
	}
	l.recheck = func(ctx context.Context) error { return refuseFormat(ctx, dockerCli, name, o.ignoreFormat, 1) }
	var mu sync.Mutex
	changed := false
	err = withLock(ctx, dockerCli, l, false, func() error {
		if o.all {
			before, err := composeState(ctx, dockerCli, name)
			if err == nil {
				err = upPlainAndProxies(ctx, l, l.Derived.Project)
			}
			if err != nil {
				return Exit(1, err)
			}
			after, err := composeState(ctx, dockerCli, name)
			changed = err != nil || !maps.Equal(before, after)
		}
		upID := time.Now().UTC().Format("20060102T150405Z")
		return bounceAll(ctx, l, svcs, func(svc config.Service) (types.ServiceConfig, error) {
			cur, err := current(ctx, l, svc.Name)
			if err != nil {
				return types.ServiceConfig{}, err
			}
			app, err := desiredApp(ctx, l, svc, upID)
			if err == nil && (cur == nil || cur[revision.LabelSpecHash] != app.Labels[revision.LabelSpecHash]) {
				mu.Lock()
				changed = true
				mu.Unlock()
				say(api.Working, "Bouncing", svc.Name)
			}
			return app, err
		})
	})
	if held, ok := errors.AsType[lock.ErrHeld](err); ok {
		return skip("bounce in progress by " + held.Owner)
	}
	if err != nil {
		return fail(err)
	}
	if changed {
		say(api.Done, "Refreshed")
		return refreshResult{status: refreshed}
	}
	say(api.Done, "Up to date")
	return refreshResult{status: upToDate}
}

// lockHeld reports a live lock on l's project, as lock.Acquire would find
// it, and its owner.
func lockHeld(ctx context.Context, dockerCli command.Cli, l *loaded) (string, bool) {
	id, created, labels, err := lock.NewDocker(dockerCli.Client()).Inspect(ctx, l.Derived.Project.Name+"-bouncer-lock")
	return labels[lock.LabelLockOwner], err == nil && id != "" && time.Since(created) < staleAfter(l)
}

// plainServices are the derived project's services that are neither a
// Service's proxy nor its replicas.
func plainServices(l *loaded) []string {
	bouncer := map[string]bool{}
	for _, s := range l.Derived.Services {
		bouncer[s.Name], bouncer[transform.AppName(s.Name)] = true, true
	}
	var out []string
	for n := range l.Derived.Project.Services {
		if !bouncer[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// composeState maps the project's containers that Compose's up converges
// (plain services and proxies) to their ID and state: a change after it
// means one was created, recreated or started.
func composeState(ctx context.Context, dockerCli command.Cli, project string) (map[string]string, error) {
	res, err := dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{All: true,
		Filters: client.Filters{}.Add("label", api.ProjectLabel+"="+project)})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range res.Items {
		if r := c.Labels[transform.LabelRole]; (r == "" || r == transform.RoleProxy) && len(c.Names) > 0 {
			out[c.Names[0]] = c.ID + " " + string(c.State)
		}
	}
	return out, nil
}

// refreshPull pulls the targets' tagged images, as pull_policy always, and
// returns those that now name another image. Digest references can't move;
// build: services are not rebuilt by refresh; pull_policy never and build are
// honoured.
func refreshPull(ctx context.Context, l *loaded, targets types.Services, present func(context.Context, string) bool) ([]string, error) {
	pulls := types.Services{}
	before := map[string]string{}
	for n, s := range targets {
		switch {
		case s.Build != nil:
			l.event("Service "+original(l, n), api.Warning, "Not pulled:", "build: services are not rebuilt by refresh")
			continue
		case strings.Contains(s.Image, "@"), s.PullPolicy == types.PullPolicyNever, s.PullPolicy == types.PullPolicyBuild:
			continue
		}
		s.PullPolicy = types.PullPolicyAlways
		pulls[n] = s
		before[s.Image], _, _ = l.Engine.Image(ctx, s.Image) // "" when missing
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	if err := prePull(ctx, l, pulls, "", false, present); err != nil {
		return nil, err
	}
	var moved []string
	for ref, id := range before {
		if now, _, _ := l.Engine.Image(ctx, ref); now != id {
			moved = append(moved, ref)
		}
	}
	sort.Strings(moved)
	return moved, nil
}

// refreshDryRun reports, per Service, what a refresh would change, and
// whether it would change anything. Nothing is pulled: a tag that moved in
// the registry is found by its manifest digest.
func refreshDryRun(ctx context.Context, l *loaded, svcs []config.Service, o refreshOptions) bool {
	change := false
	for _, svc := range svcs {
		id := "Service " + svc.Name
		app := l.Derived.Project.Services[transform.AppName(svc.Name)]
		var why []string
		if d := registryMoved(ctx, l, id, app, o); d != "" {
			why = append(why, fmt.Sprintf("%s moved in the registry to %s", app.Image, d))
		}
		cur, err := current(ctx, l, svc.Name)
		pinned, perr := pinImage(ctx, l, app)
		switch {
		case err != nil:
			why = append(why, err.Error())
		case perr != nil:
			why = append(why, perr.Error())
		case cur == nil:
			why = append(why, "no running replica")
		default:
			if _, hash, err := revision.Encode(pinned); err == nil && hash != cur[revision.LabelSpecHash] {
				if from, to := cur[labelImage], pinned.Labels[labelImage]; from != to {
					why = append(why, fmt.Sprintf("image %s moved from %s to %s", app.Image, shortImage(from), shortImage(to)))
				} else {
					why = append(why, "definition changed")
				}
			}
		}
		if len(why) == 0 {
			l.event(id, api.Done, "Up to date")
			continue
		}
		change = true
		l.event(id, api.Warning, "Would bounce:", strings.Join(why, "; "))
	}
	// Plain services and proxies, as upPlainAndProxies hands them to Compose.
	p := noPull(l.Derived.Project.WithServicesDisabled(appNames(l)...))
	for _, n := range slices.Sorted(maps.Keys(p.Services)) {
		verb, why := composeChange(ctx, l, p.Services[n], o)
		if verb == "" {
			continue
		}
		text := "Would " + verb + " with -a:"
		if o.all {
			text, change = "Would "+verb+":", true
		}
		l.event("Service "+n, api.Warning, text, why)
	}
	return change
}

// composeChange is what Compose's up would do to s's container, as
// recreate-on-diverged does: "create" when there is none, "recreate" when
// its config hash (compose.ServiceHash) or, for a plain service, its image
// changed, "start" when it is stopped; "" when nothing.
func composeChange(ctx context.Context, l *loaded, s types.ServiceConfig, o refreshOptions) (verb, why string) {
	c, err := l.Engine.Container(ctx, l.Derived.Project.Name, map[string]string{api.ServiceLabel: s.Name})
	switch {
	case err != nil:
		return "", ""
	case c == nil:
		return "create", "no container"
	}
	if h, err := compose.ServiceHash(s); err == nil && h != c.Labels[api.ConfigHashLabel] {
		return "recreate", "definition changed"
	}
	if _, proxy := c.Labels[transform.LabelRole]; !proxy && s.Build == nil {
		if d := registryMoved(ctx, l, "Service "+s.Name, s, o); d != "" {
			return "recreate", fmt.Sprintf("%s moved in the registry to %s", s.Image, d)
		}
		if local, _, err := l.Engine.Image(ctx, s.Image); err == nil && local != c.Labels[api.ImageDigestLabel] {
			return "recreate", fmt.Sprintf("image %s moved from %s to %s", s.Image, shortImage(c.Labels[api.ImageDigestLabel]), shortImage(local))
		}
	}
	if !c.Running {
		return "start", "stopped"
	}
	return "", ""
}

// registryMoved is ref@digest when the registry's image for s's tag is not
// the local one; "" when it is, or there is nothing to ask.
func registryMoved(ctx context.Context, l *loaded, id string, s types.ServiceConfig, o refreshOptions) string {
	if o.pull == types.PullPolicyNever || s.Build != nil || strings.Contains(s.Image, "@") {
		return ""
	}
	named, err := reference.ParseNormalizedNamed(s.Image)
	if err != nil {
		return ""
	}
	d, err := o.registryDigest(ctx, s.Image)
	if err != nil {
		l.event(id, api.Warning, "Registry lookup failed:", err.Error())
		return ""
	}
	_, local, _ := l.Engine.Image(ctx, s.Image)
	for _, rd := range local {
		if c, err := reference.ParseNormalizedNamed(rd); err == nil && c.Name() == named.Name() {
			if cd, ok := c.(reference.Digested); ok && cd.Digest().String() == d {
				return ""
			}
		}
	}
	return reference.FamiliarName(named) + "@" + d
}

// shortImage shortens an image ID for a message; repo@digest stays whole.
func shortImage(s string) string {
	if id, ok := strings.CutPrefix(s, "sha256:"); ok && len(id) > 12 {
		return id[:12]
	}
	return s
}

// shellVars are the variables the compose files use without a default that
// the env files (without any, the project's .env) don't set: they came from
// the shell at the last up, and refresh can't replay them.
func shellVars(inv invocation) []string {
	files := inv.EnvFiles
	if dotEnv := filepath.Join(inv.Dir, ".env"); len(files) == 0 {
		if _, err := os.Stat(dotEnv); err == nil {
			files = []string{dotEnv}
		}
	}
	env, _ := dotenv.GetEnvFromFile(map[string]string{}, files)
	var out []string
	for _, f := range inv.Files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for name, v := range template.ExtractVariables(map[string]any{"": string(b)}, nil) {
			if _, set := env[name]; !set && v.DefaultValue == "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
