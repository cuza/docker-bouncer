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
	pulled       map[string]string // service -> pull_policy
	ups          int
	providerRuns int
	lastEnv      types.MappingWithEquals // api-app's in the last Up
	built        []string
	pull         bool           // BuildOptions.Pull of the last Build
	bp           *types.Project // the project of the last Build
}

func (f *fakeCompose) Build(_ context.Context, p *types.Project, o api.BuildOptions) error {
	f.built, f.pull, f.bp = o.Services, o.Pull, p
	slices.Sort(f.built)
	return nil
}

func (f *fakeCompose) Pull(_ context.Context, p *types.Project, _ api.PullOptions) error {
	f.pulled = map[string]string{}
	for n, s := range p.Services {
		f.pulled[n] = s.PullPolicy
	}
	return nil
}

func (f *fakeCompose) Create(context.Context, *types.Project, api.CreateOptions) error { return nil }

// Up runs the project's providers as Compose does: it injects URL into
// their dependents' environment.
func (f *fakeCompose) Up(_ context.Context, p *types.Project, _ api.UpOptions) error {
	f.ups++ // called under loaded.mu
	for name, s := range p.Services {
		if s.Provider == nil {
			continue
		}
		f.providerRuns++
		for _, d := range p.Services {
			if _, ok := d.DependsOn[name]; ok {
				d.Environment["URL"] = new("from-" + name)
			}
		}
	}
	f.lastEnv = p.Services["api-app"].Environment
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

// present holds images as "ref" (any platform) or "ref platform".
func present(images ...string) func(context.Context, string, string) bool {
	return func(_ context.Context, image, platform string) bool {
		return slices.Contains(images, image) || slices.Contains(images, image+" "+platform)
	}
}

// A service with platform: is pulled unless the image is local for that platform.
func TestPrePullChecksPlatform(t *testing.T) {
	amd := service("tool", "registry/tool:1", "", false)
	amd.Platform = "linux/amd64"
	l := derive(t, amd, service("db", "registry/tool:1", "", false))
	have := func(_ context.Context, image, platform string) bool { return platform == "" }
	if err := prePull(context.Background(), l, l.Derived.Project.Services, "", true, have); err != nil {
		t.Fatal(err)
	}
	if f := l.Compose.(*fakeCompose); len(f.pulled) != 1 || f.pulled["tool"] == "" {
		t.Fatalf("pulled %v, want tool for its platform", f.pulled)
	}
}

func TestPrePullSelection(t *testing.T) {
	l := derive(t,
		service("api", "registry/api:1", types.PullPolicyMissing, true),  // missing: proxy present, api-app pulled
		service("db", "registry/db:1", types.PullPolicyAlways, false),    // present but always
		service("cache", "registry/cache:1", "", false),                  // present, default missing
		service("tool", "registry/tool:1", types.PullPolicyNever, false), // absent but never
	)
	have := present(config.DefaultProxyImage, "registry/db:1", "registry/cache:1")
	if err := prePull(context.Background(), l, l.Derived.Project.Services, "", true, have); err != nil {
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
	if err := prePull(context.Background(), l, l.Derived.Project.Services, types.PullPolicyNever, true, present()); err != nil || f.pulled != nil {
		t.Fatalf("--pull never pulled %v (%v)", f.pulled, err)
	}
}

func TestPrePullLockImageWithoutServices(t *testing.T) {
	l := derive(t, service("db", "registry/db:1", "", false))
	if err := prePull(context.Background(), l, l.Derived.Project.Services, "", true, present("registry/db:1")); err != nil {
		t.Fatal(err)
	}
	if f := l.Compose.(*fakeCompose); len(f.pulled) != 1 || f.pulled["bouncer-lock"] != types.PullPolicyAlways {
		t.Fatalf("pulled %v", f.pulled)
	}
}

// up builds every service with build: and pulls none of them (--pull always
// pulls their base images); pull pulls one only when it has an image:, as
// docker compose pull. build builds the user's services, not the proxies
// named after them.
func TestBuildServices(t *testing.T) {
	built := service("api", "", "", true)
	built.Build = &types.BuildConfig{Context: "."}
	tool := service("tool", "", "", false)
	tool.Build = &types.BuildConfig{Context: "."}
	pushed := service("web", "registry/web:1", "", true)
	pushed.Build = &types.BuildConfig{Context: "."}
	l := derive(t, built, tool, pushed, service("db", "registry/db:1", "", false))
	f := l.Compose.(*fakeCompose)
	pulled := func(skipBuilt bool) []string {
		t.Helper()
		if err := prePull(context.Background(), l, l.Derived.Project.Services, types.PullPolicyAlways, skipBuilt, present()); err != nil {
			t.Fatal(err)
		}
		var out []string
		for n := range f.pulled {
			out = append(out, n)
		}
		slices.Sort(out)
		return out
	}
	if got := pulled(true); !slices.Equal(got, []string{"api", "db", "web"}) { // the proxies, db; the lock shares the proxy image
		t.Fatalf("up pulled %v", got)
	}
	if got := pulled(false); !slices.Equal(got, []string{"api", "db", "web", "web-app"}) {
		t.Fatalf("pull pulled %v, want web-app (it has image:) but not api-app or tool", got)
	}

	if err := build(context.Background(), l, l.Derived.Project, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.built, []string{"api", "tool", "web"}) || !f.pull {
		t.Fatalf("built %v pull %v", f.built, f.pull)
	}
	if s := f.bp.Services["api"]; s.Build == nil || s.Image != "" {
		t.Fatalf("build must see the user's api, not its proxy: %+v", s)
	}
	if f.bp == l.Project {
		t.Fatal("build must hand Compose a copy of the user's project")
	}
	if l.Derived.Project.Services["api-app"].Image != "proj-api" || l.Derived.Project.Services["web-app"].Image != "registry/web:1" {
		t.Fatal("the replicas must run the image Compose builds: <project>-<service>, or image:")
	}
}

// Run with -race: parallel bounces must not write into the shared project.
func TestScaleUpConcurrentServices(t *testing.T) {
	l := derive(t, service("api", "registry/api:1", "", true), service("web", "registry/web:1", "", true))
	var wg sync.WaitGroup
	for _, name := range []string{"api-app", "web-app"} {
		wg.Go(func() {
			for total := 3; total < 30; total++ {
				if err := (composeScaler{l: l}).ScaleUp(context.Background(), l.Derived.Project.Services[name], total); err != nil {
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

// Every step of a bounce that creates replicas runs their providers, so
// the new replicas get the injected environment.
func TestScaleUpRunsProviders(t *testing.T) {
	db := types.ServiceConfig{Name: "db", Provider: &types.ServiceProviderConfig{Type: "model"}}
	api := service("api", "registry/api:1", "", true)
	api.DependsOn = types.DependsOnConfig{"db": {Condition: types.ServiceConditionStarted, Required: true}}
	l := derive(t, db, api)
	f := l.Compose.(*fakeCompose)
	sc := composeScaler{l: l}
	for total := 1; total <= 3; total++ {
		if err := sc.ScaleUp(context.Background(), l.Derived.Project.Services["api-app"], total); err != nil {
			t.Fatal(err)
		}
		if v := f.lastEnv["URL"]; v == nil || *v != "from-db" {
			t.Fatalf("step %d: replicas created without the provider's environment: %v", total, f.lastEnv)
		}
	}
	if f.providerRuns != 3 {
		t.Fatalf("providers ran %d times in three steps, want 3", f.providerRuns)
	}
	if _, leaked := l.Derived.Project.Services["api-app"].Environment["URL"]; leaked {
		t.Fatal("the provider's environment must not leak into the derived project")
	}
}
