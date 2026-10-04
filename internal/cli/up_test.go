package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	reps []engine.Replica
	id   string // ImageID's answer; "sha256:<ref>" when empty
}

func (f *fakeEngine) Replicas(context.Context, string, string) ([]engine.Replica, error) {
	return f.reps, nil
}
func (f *fakeEngine) Container(context.Context, string, map[string]string) (*engine.Replica, error) {
	return nil, nil
}
func (f *fakeEngine) Stop(context.Context, string) error   { return nil }
func (f *fakeEngine) Remove(context.Context, string) error { return nil }
func (f *fakeEngine) Exec(context.Context, string, []string, ...string) (string, error) {
	return "", nil
}
func (f *fakeEngine) Wait(context.Context, string, time.Duration) {}
func (f *fakeEngine) ImageID(_ context.Context, ref string) (string, error) {
	if f.id != "" {
		return f.id, nil
	}
	return "sha256:" + ref, nil
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

// A built Service's replicas run the image ID, so a rebuild is a new revision.
func TestDesiredAppPinsBuiltImage(t *testing.T) {
	svc := service("api", "", "", true)
	svc.Build = &types.BuildConfig{Context: "."}
	l := derive(t, svc)
	l.Engine = &fakeEngine{}
	app, err := desiredApp(context.Background(), l, l.Derived.Services[0], "u1")
	if err != nil || app.Image != "sha256:proj-api" {
		t.Fatalf("image %q %v", app.Image, err)
	}
	l.Engine.(*fakeEngine).reps = []engine.Replica{{Name: "proj-api-app-1", Running: true, Labels: app.Labels}}
	if again, _ := desiredApp(context.Background(), l, l.Derived.Services[0], "u2"); again.Labels[revision.LabelRevision] != "1" {
		t.Fatal("the same image is the same revision")
	}
	l.Engine.(*fakeEngine).id = "sha256:rebuilt"
	if next, _ := desiredApp(context.Background(), l, l.Derived.Services[0], "u3"); next.Labels[revision.LabelRevision] != "2" || next.Image != "sha256:rebuilt" {
		t.Fatalf("a rebuilt image is a new revision: %v %q", next.Labels[revision.LabelRevision], next.Image)
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
