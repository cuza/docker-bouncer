package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	dockercli "github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
)

type fakeEngine struct {
	reps    []engine.Replica
	id      string          // Image's ID; "sha256:<ref>" when empty
	digests []string        // Image's repo digests
	hook    *engine.Replica // what Container returns
	kept    []string        // RemoveKeepVolumes calls
	logs    string
}

func (f *fakeEngine) Replicas(context.Context, string, string) ([]engine.Replica, error) {
	return f.reps, nil
}
func (f *fakeEngine) Container(context.Context, string, map[string]string) (*engine.Replica, error) {
	return f.hook, nil
}
func (f *fakeEngine) RemoveKeepVolumes(_ context.Context, id string) error {
	f.kept = append(f.kept, id)
	return nil
}
func (f *fakeEngine) Logs(context.Context, string, int) (string, error) { return f.logs, nil }
func (f *fakeEngine) Start(context.Context, string) error               { return nil }
func (f *fakeEngine) Stop(context.Context, string) error                { return nil }
func (f *fakeEngine) Remove(context.Context, string) error              { return nil }
func (f *fakeEngine) Exec(context.Context, string, []string, ...string) (string, error) {
	return "", nil
}
func (f *fakeEngine) Wait(context.Context, string, time.Duration) {}
func (f *fakeEngine) Image(_ context.Context, ref string) (string, []string, error) {
	if f.id != "" {
		return f.id, f.digests, nil
	}
	return "sha256:" + ref, f.digests, nil
}

func fakeLoaded(t *testing.T) *loaded {
	t.Helper()
	svc := types.ServiceConfig{Name: "api", Extensions: types.Extensions{"x-bouncer": map[string]any{}}}
	svc.Image = "registry/api@sha256:1"
	svc.Expose = types.StringOrNumberList{"8080"}
	p := &types.Project{Name: "proj", Services: types.Services{"api": svc}}
	d, err := transform.Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	return &loaded{Project: p, Derived: d, Engine: &fakeEngine{}, mu: &sync.Mutex{}}
}

func TestDesiredAppReusesRunningRevision(t *testing.T) {
	l := fakeLoaded(t) // derived project with Service "api"; fake engine with no replicas
	app, err := desiredApp(context.Background(), l, l.Derived.Services[0], "u1")
	if err != nil || app.Labels[revision.LabelRevision] != "1" {
		t.Fatalf("first up stamps revision 1: %v %v", app.Labels, err)
	}
	l.Engine.(*fakeEngine).reps = []engine.Replica{{Name: "proj-api-app-1", Running: true, Labels: app.Labels}}
	again, err := desiredApp(context.Background(), l, l.Derived.Services[0], "u2")
	if err != nil || again.Labels[revision.LabelRevision] != "1" || again.Labels[revision.LabelUpID] != "u1" {
		t.Fatalf("unchanged file must reuse revision 1: %v %v", again.Labels, err)
	}
	if _, leaked := l.Derived.Project.Services["api-app"].Labels[revision.LabelRevision]; leaked {
		t.Fatal("desiredApp must not write into the derived project")
	}
}

const (
	d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// pinImage keeps the reference and labels the exact image: repo@digest for
// the reference's repository, else the image ID; a digest as written; always
// the ID for a build.
func TestPinImage(t *testing.T) {
	ctx := context.Background()
	l := derive(t, service("api", "vaultwarden/server:latest", "", true))
	fe := &fakeEngine{}
	l.Engine = fe
	app := l.Derived.Project.Services["api-app"]
	pin := func(app types.ServiceConfig) string {
		t.Helper()
		got, err := pinImage(ctx, l, app)
		if err != nil || got.Image != app.Image {
			t.Fatalf("image %q %v, want %q kept", got.Image, err, app.Image)
		}
		return got.Labels[labelImage]
	}
	fe.digests = []string{"other/mirror@" + d2, "docker.io/vaultwarden/server@" + d1}
	if got := pin(app); got != "vaultwarden/server@"+d1 {
		t.Fatalf("repo digest: %q", got)
	}
	fe.digests = []string{"other/mirror@" + d2} // only tagged locally, or pulled from elsewhere
	if got := pin(app); got != "sha256:vaultwarden/server:latest" {
		t.Fatalf("local image: %q", got)
	}
	digested := app
	digested.Image = "registry:3.1.1@sha256:1be5"
	if got := pin(digested); got != digested.Image {
		t.Fatalf("a digest in the file is exact as written: %q", got)
	}
	built := app
	built.Image, built.Build = "proj-api", &types.BuildConfig{Context: "."}
	fe.digests = []string{"proj-api@" + d1}
	if got := pin(built); got != "sha256:proj-api" {
		t.Fatalf("a build pins its image ID: %q", got)
	}
	if _, leaked := l.Derived.Project.Services["api-app"].Labels[labelImage]; leaked {
		t.Fatal("pinImage must not write into the derived project")
	}
}

// The spec hash changes only when the reference resolves to another image:
// a moved tag or a rebuild is a new revision, the same image is not.
func TestDesiredAppBouncesOnlyOnNewImage(t *testing.T) {
	for name, svc := range map[string]types.ServiceConfig{
		"pulled": service("api", "registry/api:latest", "", true),
		"built":  service("api", "", "", true),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "built" {
				svc.Build = &types.BuildConfig{Context: "."}
			}
			l := derive(t, svc)
			fe := &fakeEngine{id: "sha256:old", digests: []string{"registry/api@" + d1}}
			l.Engine = fe
			up := func(upID, wantRev string) types.ServiceConfig {
				t.Helper()
				app, err := desiredApp(context.Background(), l, l.Derived.Services[0], upID)
				if err != nil || app.Labels[revision.LabelRevision] != wantRev || app.Image != l.Derived.Project.Services["api-app"].Image {
					t.Fatalf("%s: revision %q image %q %v, want revision %s and the reference kept", upID, app.Labels[revision.LabelRevision], app.Image, err, wantRev)
				}
				fe.reps = []engine.Replica{{Name: "proj-api-app-1", Running: true, Labels: app.Labels}}
				return app
			}
			up("u1", "1")
			up("u2", "1") // same image
			fe.id, fe.digests = "sha256:new", []string{"registry/api@" + d2}
			if app := up("u3", "2"); name == "built" && app.Labels[labelImage] != "sha256:new" {
				t.Fatalf("rebuilt image %q", app.Labels[labelImage])
			}
		})
	}
}

// A restored spec runs its exact image; one stored before pinning, its reference.
func TestRunImage(t *testing.T) {
	var app types.ServiceConfig
	app.Image, app.Labels = "registry/api:1", types.Labels{labelImage: "registry/api@" + d1}
	if got := runImage(app); got != "registry/api@"+d1 {
		t.Fatal(got)
	}
	app.Labels = nil
	if got := runImage(app); got != "registry/api:1" {
		t.Fatal(got)
	}
}

// compose's CLI registers the `raw` env_file format; the plugin relies on
// revision's registration instead. Derived services carry their own Compose
// service label, or Compose cannot find their containers.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("secrets.env", "TOKEN=a$b\n")
	write("compose.yaml", `services:
  api:
    image: registry/api:1
    expose: ["8080"]
    env_file: [{path: secrets.env, format: raw}]
    x-bouncer: {}
`)
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}
	if err := dockerCli.Initialize(flags.NewClientOptions()); err != nil {
		t.Fatal(err)
	}
	l, err := load(context.Background(), dockerCli, &ProjectFlags{Files: []string{filepath.Join(dir, "compose.yaml")}})
	if err != nil {
		t.Fatal(err)
	}
	if v := l.Derived.Project.Services["api-app"].Environment["TOKEN"]; v == nil || *v != "a$b" {
		t.Fatalf("raw env_file value: %v", v)
	}
	for _, name := range []string{"api", "api-app"} {
		if got := l.Derived.Project.Services[name].CustomLabels[api.ServiceLabel]; got != name {
			t.Errorf("%s: compose service label %q", name, got)
		}
	}
}

func TestUpProjectScopesToNamedServices(t *testing.T) {
	svc := func(name string, bouncer bool, deps ...string) types.ServiceConfig {
		s := types.ServiceConfig{Name: name}
		s.Image, s.Expose = "registry/"+name+":1", types.StringOrNumberList{"8080"}
		if bouncer {
			s.Extensions = types.Extensions{"x-bouncer": map[string]any{}}
		}
		if len(deps) > 0 {
			s.DependsOn = types.DependsOnConfig{}
			for _, d := range deps {
				s.DependsOn[d] = types.ServiceDependency{Condition: "service_started"}
			}
		}
		return s
	}
	p := &types.Project{Name: "proj", Services: types.Services{
		"api": svc("api", true, "db"), "worker": svc("worker", true), "db": svc("db", false),
	}}
	d, err := transform.Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	l := &loaded{Project: p, Derived: d, Engine: &fakeEngine{}, mu: &sync.Mutex{}}

	sel, err := upProject(l, []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	got := sel.ServiceNames()
	if !slices.Equal(got, []string{"api", "api-app", "db"}) {
		t.Fatalf("up api converges %v, want api, api-app and its dependency db", got)
	}
	if all, err := upProject(l, nil); err != nil || len(all.Services) != 5 {
		t.Fatalf("up without args converges everything: %v %v", all.ServiceNames(), err)
	}
	var se dockercli.StatusError
	if _, err := upProject(l, []string{"nope"}); !errors.As(err, &se) || se.StatusCode != 2 {
		t.Fatalf("unknown service: %v, want exit 2", err)
	}
}

// down takes inactive Services' proxies and replicas, never inactive plain services.
func TestDownProject(t *testing.T) {
	api := service("api", "registry/api:1", "", true)
	api.Profiles = []string{"database"}
	api.DependsOn = types.DependsOnConfig{"db": {Condition: "service_started"}}
	db := service("db", "registry/db:1", "", false)
	db.Profiles = []string{"database"}
	p := &types.Project{Name: "proj", Services: types.Services{"web": service("web", "registry/web:1", "", false)},
		DisabledServices: types.Services{"api": api, "db": db}}
	d, err := transform.Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	got := downProject(d)
	if !slices.Equal(got.ServiceNames(), []string{"api", "api-app", "web"}) || !slices.Equal(got.DisabledServiceNames(), []string{"db"}) {
		t.Fatalf("services %v disabled %v", got.ServiceNames(), got.DisabledServiceNames())
	}
	if _, ok := got.Services["api-app"].DependsOn["db"]; ok {
		t.Fatal("an edge to a disabled service must go")
	}
	if len(d.Project.Services) != 1 || len(d.Project.Services["web"].DependsOn) != 0 || d.Project.DisabledServices["api-app"].DependsOn["db"].Condition == "" {
		t.Fatal("the derived project must not change")
	}
}

// A pre_start hook's container is removed, its volumes kept, whether it
// fails (its exit code and output are reported) or the run is cancelled.
func TestPreStartHookIsAlwaysRemoved(t *testing.T) {
	for name, tc := range map[string]struct {
		running bool
		cancel  bool
		want    string
	}{
		"fails":     {false, false, "api-app pre_start[0]: exited with 3:\nmigration failed"},
		"cancelled": {true, true, "context canceled"},
	} {
		t.Run(name, func(t *testing.T) {
			api := service("api", "registry/api:1", "", true)
			api.PreStart = []types.PreStartHook{{}}
			l := derive(t, api)
			app := l.Derived.Project.Services["api-app"]
			fe := &fakeEngine{
				reps: []engine.Replica{{ID: "new", Labels: map[string]string{revision.LabelSpecHash: "h"}}},
				hook: &engine.Replica{ID: "hook", Running: tc.running, ExitCode: 3},
				logs: "migration failed\n",
			}
			l.Engine = fe
			app.Labels = types.Labels{revision.LabelSpecHash: "h"}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				cancel()
			}
			defer cancel()
			err := (composeScaler{l: l}).preStart(ctx, l.Derived.Project, app)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if !slices.Contains(fe.kept, "hook") {
				t.Fatalf("hook container not removed (keeping volumes): %v", fe.kept)
			}
		})
	}
}
