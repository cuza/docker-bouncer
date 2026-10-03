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

type fakeEngine struct{ reps []engine.Replica }

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
