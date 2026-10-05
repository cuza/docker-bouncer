package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	dockercli "github.com/docker/cli/cli"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

func TestInvocationRoundTrip(t *testing.T) {
	inv := invocation{Version: "v1.2.3", Command: "undo", Revision: 4, Files: []string{"/srv/app/compose.yaml", "/srv/app/prod.yaml"},
		Dir: "/srv/app", Name: "app", EnvFiles: []string{"/srv/app/prod.env"}, Profiles: []string{"*"}}
	got, ok, err := decodeInvocation(map[string]string{transform.LabelInvocation: inv.encode()})
	if err != nil || !ok || !reflect.DeepEqual(got, inv) {
		t.Fatalf("%+v %v %v, want %+v", got, ok, err, inv)
	}
	if _, ok, err := decodeInvocation(nil); ok || err != nil {
		t.Fatalf("no label: ok=%v err=%v", ok, err)
	}
	if _, _, err := decodeInvocation(map[string]string{transform.LabelInvocation: "not base64!"}); err == nil {
		t.Fatal("a bad label is an error")
	}
}

func TestNewInvocationMakesEnvFilesAbsolute(t *testing.T) {
	p := &types.Project{Name: "app", WorkingDir: "/srv/app", ComposeFiles: []string{"/srv/app/compose.yaml"}, Profiles: []string{""}}
	inv := newInvocation(p, &ProjectFlags{EnvFiles: []string{"x.env"}})
	wd, _ := os.Getwd()
	if inv.Command != "up" || !slices.Equal(inv.EnvFiles, []string{filepath.Join(wd, "x.env")}) || inv.Profiles != nil {
		t.Fatalf("%+v", inv)
	}
}

// The invocation is bookkeeping: an up from another directory or with
// another version never bounces.
func TestInvocationNotInSpecHash(t *testing.T) {
	app := types.ServiceConfig{Name: "api-app"}
	app.Image, app.Labels = "registry/app:1", types.Labels{"a": "b"}
	_, want, err := revision.Encode(app)
	if err != nil {
		t.Fatal(err)
	}
	app.Labels = types.Labels{"a": "b", transform.LabelInvocation: invocation{Command: "up", Dir: "/elsewhere"}.encode(),
		transform.LabelVersion: "v9.9.9", transform.LabelFormat: "1.7"} // a CLI upgrade alone never bounces
	if _, got, _ := revision.Encode(app); got != want {
		t.Fatalf("hash %s with the bookkeeping labels, %s without", got, want)
	}
}

func TestPickProjects(t *testing.T) {
	found := map[string]hostProject{"web": {}, "api": {}}
	if got, err := pickProjects(found, nil); err != nil || !slices.Equal(got, []string{"api", "web"}) {
		t.Fatalf("all: %v %v", got, err)
	}
	if got, err := pickProjects(found, []string{"web", "web"}); err != nil || !slices.Equal(got, []string{"web"}) {
		t.Fatalf("named: %v %v", got, err)
	}
	_, err := pickProjects(found, []string{"worker"})
	if se, ok := errors.AsType[dockercli.StatusError](err); !ok || se.StatusCode != 2 {
		t.Fatalf("unknown project: %v, want exit 2", err)
	}
}

func TestRefreshSummary(t *testing.T) {
	for _, tc := range []struct {
		res  []refreshResult
		text string
		code int
	}{
		{nil, "0 projects", 0},
		{[]refreshResult{{status: refreshed}, {status: upToDate}, {status: skipped}}, "3 projects: 1 refreshed, 1 up to date, 1 skipped", 0},
		{[]refreshResult{{status: upToDate}, {status: failed, err: Exit(1, errors.New("x"))}}, "2 projects: 1 up to date, 1 failed", 1},
		{[]refreshResult{{status: failed, err: errors.New("x")}, {status: failed, err: Exit(2, errors.New("y"))}}, "2 projects: 2 failed", 2},
		{[]refreshResult{{status: wouldDo}}, "1 project: 1 would change", 0},
	} {
		text, err := refreshSummary("refresh", tc.res)
		code := 0
		if se, ok := errors.AsType[dockercli.StatusError](err); ok {
			code = se.StatusCode
		}
		if text != tc.text || code != tc.code {
			t.Errorf("%v: %q exit %d, want %q exit %d", tc.res, text, code, tc.text, tc.code)
		}
	}
	if _, err := refreshSummary("migrate", []refreshResult{{status: failed, err: errors.New("x")}}); err == nil || err.Error() != "migrate: 1 of 1 project failed" {
		t.Fatalf("migrate summary: %v", err)
	}
}

func TestShellVars(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yaml")
	os.WriteFile(compose, []byte("services:\n  api:\n    image: registry/app:${TAG}\n    environment: [\"A=${FROM_ENV}\", \"B=${DEF:-x}\", \"C=$$LITERAL\"]\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("FROM_ENV=1\n"), 0o644)
	inv := invocation{Files: []string{compose}, Dir: dir}
	if got := shellVars(inv); !slices.Equal(got, []string{"TAG"}) {
		t.Fatalf(".env: %v, want [TAG]", got)
	}
	other := filepath.Join(dir, "x.env") // an explicit env file replaces .env
	os.WriteFile(other, []byte("TAG=1\n"), 0o644)
	inv.EnvFiles = []string{other}
	if got := shellVars(inv); !slices.Equal(got, []string{"FROM_ENV"}) {
		t.Fatalf("--env-file: %v, want [FROM_ENV]", got)
	}
}

type recorder struct {
	api.EventProcessor
	lines []string
}

func (r *recorder) On(events ...api.Resource) {
	for _, e := range events {
		r.lines = append(r.lines, e.ID+" "+e.Text+" "+e.Details)
	}
}

// The dry run models Compose's up for plain services and proxies: create,
// start, or recreate on a config-hash or image change.
func TestDryRunPlainAndProxies(t *testing.T) {
	l := derive(t, service("api", "registry/app:1", "", true), service("db", "registry/db:1", "", false),
		service("cache", "registry/cache:1", "", false), service("queue", "registry/queue:1", "", false), service("web", "registry/web:1", "", false))
	p := noPull(l.Derived.Project.WithServicesDisabled(appNames(l)...))
	hash := func(n string) string { h, _ := compose.ServiceHash(p.Services[n]); return h }
	at := func(n string, running bool, labels map[string]string) *engine.Replica {
		labels[api.ConfigHashLabel] = hash(n)
		return &engine.Replica{Running: running, Labels: labels, ImageID: "sha256:" + n}
	}
	fe := &fakeEngine{images: map[string]string{"registry/db:1": "sha256:db", "registry/cache:1": "sha256:cache", "registry/queue:1": "sha256:new"},
		containers: map[string]*engine.Replica{
			"api":   at("api", true, map[string]string{transform.LabelRole: transform.RoleProxy}),
			"db":    at("db", true, map[string]string{}),     // converged
			"cache": at("cache", false, map[string]string{}), // stopped
			"queue": at("queue", true, map[string]string{}),  // its image moved
			// web: no container
		}}
	fe.containers["api"].Labels[api.ConfigHashLabel] = "old" // proxy definition changed
	l.Engine = fe
	for _, all := range []bool{false, true} {
		r := &recorder{}
		l.Events = r
		change, err := refreshDryRun(context.Background(), l, nil, refreshOptions{all: all, pull: types.PullPolicyNever})
		if err != nil {
			t.Fatal(err)
		}
		suffix := " with -a:"
		if all {
			suffix = ":"
		}
		want := []string{
			"Service api Would recreate" + suffix + " definition changed",
			"Service cache Would start" + suffix + " stopped",
			"Service queue Would recreate" + suffix + " image registry/queue:1 moved from sha256:queue to sha256:new",
			"Service web Would create" + suffix + " no container",
		}
		if strings.Join(r.lines, "\n") != strings.Join(want, "\n") || change != all {
			t.Errorf("all=%v: change %v\n%s\nwant\n%s", all, change, strings.Join(r.lines, "\n"), strings.Join(want, "\n"))
		}
	}
}

// The dry run asks the registry only for what refresh would pull.
func TestRegistryMovedHonoursPullPolicy(t *testing.T) {
	l := derive(t, service("api", "registry/app:1", "", false))
	l.Engine, l.Events = &fakeEngine{}, &recorder{}
	var asked []string
	o := refreshOptions{registryDigest: func(_ context.Context, ref string) (string, error) {
		asked = append(asked, ref)
		return "sha256:1", nil
	}}
	for _, policy := range []string{types.PullPolicyNever, types.PullPolicyBuild, types.PullPolicyMissing, ""} {
		s := service("api", "registry/app:1", policy, false)
		registryMoved(context.Background(), l, s, o)
	}
	if len(asked) != 2 {
		t.Fatalf("asked the registry %d times, want 2 (missing and the default)", len(asked))
	}
}

// A registry that can't be asked fails the dry run: it can't report "Up to
// date" for a tag it couldn't check.
func TestDryRunRegistryFailure(t *testing.T) {
	l := derive(t, service("api", "registry/app:1", "", true))
	l.Engine, l.Events = &fakeEngine{}, &recorder{}
	o := refreshOptions{registryDigest: func(context.Context, string) (string, error) { return "", errors.New("unauthorized") }}
	if _, err := refreshDryRun(context.Background(), l, l.Derived.Services, o); err == nil || !strings.Contains(err.Error(), "registry lookup for registry/app:1: unauthorized") {
		t.Fatalf("%v, want the registry error", err)
	}
}
