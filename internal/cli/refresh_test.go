package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/cuza/docker-bouncer/internal/transform"
	dockercli "github.com/docker/cli/cli"
)

func TestInvocationRoundTrip(t *testing.T) {
	inv := invocation{Version: "v1.2.3", Command: "undo", Revision: 4, Files: []string{"/srv/app/compose.yaml", "/srv/app/prod.yaml"},
		Dir: "/srv/app", Name: "app", EnvFiles: []string{"/srv/app/prod.env"}, Profiles: []string{"*"}}
	got, ok, err := decodeInvocation(inv.encode())
	if err != nil || !ok || !reflect.DeepEqual(got, inv) {
		t.Fatalf("%+v %v %v, want %+v", got, ok, err, inv)
	}
	if _, ok, err := decodeInvocation(""); ok || err != nil {
		t.Fatalf("no label: ok=%v err=%v", ok, err)
	}
	if _, _, err := decodeInvocation("not base64!"); err == nil {
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
	app.Labels = types.Labels{"a": "b", transform.LabelInvocation: invocation{Command: "up", Dir: "/elsewhere"}.encode()}
	if _, got, _ := revision.Encode(app); got != want {
		t.Fatalf("hash %s with the invocation label, %s without", got, want)
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
		text, err := refreshSummary(tc.res)
		code := 0
		if se, ok := errors.AsType[dockercli.StatusError](err); ok {
			code = se.StatusCode
		}
		if text != tc.text || code != tc.code {
			t.Errorf("%v: %q exit %d, want %q exit %d", tc.res, text, code, tc.text, tc.code)
		}
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
