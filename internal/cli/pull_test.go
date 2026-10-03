package cli

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/compose/v5/pkg/api"
)

// fakeCompose implements the calls under test; any other panics.
type fakeCompose struct {
	api.Compose
	pulled map[string]string // service -> pull_policy
	ups    int
}

func (f *fakeCompose) Pull(_ context.Context, p *types.Project, _ api.PullOptions) error {
	f.pulled = map[string]string{}
	for n, s := range p.Services {
		f.pulled[n] = s.PullPolicy
	}
	return nil
}

func (f *fakeCompose) Up(context.Context, *types.Project, api.UpOptions) error {
	f.ups++ // called under loaded.mu
	return nil
}

func derive(t *testing.T, svcs ...types.ServiceConfig) *loaded {
	t.Helper()
	p := &types.Project{Name: "proj", Services: types.Services{}}
	for _, s := range svcs {
		p.Services[s.Name] = s
	}
	d, err := transform.Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	return &loaded{Compose: &fakeCompose{}, Project: p, Derived: d, mu: &sync.Mutex{}}
}

func service(name, image, policy string, bouncer bool) types.ServiceConfig {
	s := types.ServiceConfig{Name: name}
	s.Image, s.PullPolicy = image, policy
	if bouncer {
		s.Expose = types.StringOrNumberList{"8080"}
		s.Extensions = types.Extensions{"x-bouncer": map[string]any{}}
		s.Deploy = &types.DeployConfig{}
		s.SetScale(2)
	}
	return s
}

func present(images ...string) func(context.Context, string) bool {
	return func(_ context.Context, image string) bool { return slices.Contains(images, image) }
}

func TestPrePullSelection(t *testing.T) {
	l := derive(t,
		service("api", "registry/api:1", types.PullPolicyMissing, true),  // missing: proxy present, api-app pulled
		service("db", "registry/db:1", types.PullPolicyAlways, false),    // present but always
		service("cache", "registry/cache:1", "", false),                  // present, default missing
		service("tool", "registry/tool:1", types.PullPolicyNever, false), // absent but never
	)
	have := present(config.DefaultProxyImage, "registry/db:1", "registry/cache:1")
	if err := prePull(context.Background(), l, "", have); err != nil {
		t.Fatal(err)
	}
	f := l.Compose.(*fakeCompose)
	want := map[string]string{"api-app": types.PullPolicyAlways, "db": types.PullPolicyAlways}
	if len(f.pulled) != len(want) || f.pulled["api-app"] != want["api-app"] || f.pulled["db"] != want["db"] {
		t.Fatalf("pulled %v", f.pulled)
	}
	if l.Derived.Project.Services["api-app"].PullPolicy != types.PullPolicyMissing {
		t.Fatal("prePull must not write into the derived project")
	}

	f.pulled = nil
	if err := prePull(context.Background(), l, types.PullPolicyNever, present()); err != nil || f.pulled != nil {
		t.Fatalf("--pull never pulled %v (%v)", f.pulled, err)
	}
}

func TestPrePullLockImageWithoutServices(t *testing.T) {
	l := derive(t, service("db", "registry/db:1", "", false))
	if err := prePull(context.Background(), l, "", present("registry/db:1")); err != nil {
		t.Fatal(err)
	}
	if f := l.Compose.(*fakeCompose); len(f.pulled) != 1 || f.pulled["bouncer-lock"] != types.PullPolicyAlways {
		t.Fatalf("pulled %v", f.pulled)
	}
}

// Run with -race: parallel bounces must not write into the shared project.
func TestScaleUpConcurrentServices(t *testing.T) {
	l := derive(t, service("api", "registry/api:1", "", true), service("web", "registry/web:1", "", true))
	var wg sync.WaitGroup
	for _, name := range []string{"api-app", "web-app"} {
		wg.Go(func() {
			for total := 3; total < 30; total++ {
				if err := (composeScaler{l}).ScaleUp(context.Background(), l.Derived.Project.Services[name], total); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	for _, name := range []string{"api-app", "web-app"} {
		if s := l.Derived.Project.Services[name]; s.GetScale() != 2 || s.PullPolicy != "" {
			t.Fatalf("%s: derived project changed: scale %d pull %q", name, s.GetScale(), s.PullPolicy)
		}
	}
}
