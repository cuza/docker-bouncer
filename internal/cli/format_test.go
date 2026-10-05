package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/format"
	"github.com/cuza/docker-bouncer/internal/revision"

	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/compose/v5/pkg/api"
)

func TestFormatCheck(t *testing.T) {
	at := func(format, version string) map[string]string {
		l := map[string]string{transform.LabelRole: transform.RoleReplica}
		if format != "" {
			l[transform.LabelFormat], l[transform.LabelVersion] = format, version
		}
		return l
	}
	for _, tc := range []struct {
		name     string
		labels   []map[string]string
		mutating bool
		ignore   bool
		refuse   string // in the refusal
		note     string // in a note, a warning unless info
		info     bool
	}{
		{"same", []map[string]string{at("1.0", "v1")}, true, false, "", "", false},
		{"newer minor", []map[string]string{at("1.0", "v1"), at("1.3", "v1.5.0")}, true, false, "", "deployed with a newer bouncer (v1.5.0, format 1.3); this CLI writes 1.0", false},
		{"newer major, mutating", []map[string]string{at("1.0", "v1"), at("2.0", "v3.0.0")}, true, false, "format 2.0 from bouncer v3.0.0 is newer than this CLI", "", false},
		{"newer major, read-only", []map[string]string{at("2.0", "v3.0.0")}, false, false, "", "format 2.0 from bouncer v3.0.0 is newer than this CLI", false},
		{"newer major, ignored", []map[string]string{at("2.0", "v3.0.0")}, true, true, "", "--ignore-format: project web: format 2.0", false},
		{"unreadable major, mutating", []map[string]string{at("0.4", "v0.1.0")}, true, false, "format 0.4 is no longer read by this CLI; run docker bouncer down with bouncer v0.1.0", "", false},
		{"unreadable major, read-only", []map[string]string{at("0.4", "v0.1.0")}, false, false, "", "no longer read by this CLI", false},
		{"missing", []map[string]string{at("", "")}, true, false, "", "project web is at format none, this CLI writes 1.0; run docker bouncer migrate web", false},
		{"unparseable, mutating", []map[string]string{at("1.0", "v1"), at("v2-beta", "v3.0.0")}, true, false, `format "v2-beta" from bouncer v3.0.0 is not one this CLI understands`, "", false},
		{"unparseable, ignored", []map[string]string{at("v2-beta", "v3.0.0")}, true, true, "", `--ignore-format: project web: format "v2-beta"`, false},
		{"unparseable, read-only", []map[string]string{at("v2-beta", "v3.0.0")}, false, false, "", "is not one this CLI understands", false},
		{"older minor", []map[string]string{at("1.0", "v1")}, false, false, "", "", false}, // 1.0 is Current: no hint
		{"no replicas", nil, true, false, "", "", false},
	} {
		err, notes := formatCheck("web", tc.labels, tc.mutating, tc.ignore)
		var text []string
		for _, n := range notes {
			text = append(text, n.Text)
			if strings.Contains(n.Text, tc.note) && !tc.info && n.Status != api.Warning {
				t.Errorf("%s: %q is not a warning", tc.name, n.Text)
			}
		}
		got := strings.Join(text, "\n")
		switch {
		case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
			t.Errorf("%s: %v, want a refusal with %q", tc.name, err, tc.refuse)
		case tc.refuse == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.note == "" && len(notes) > 0, tc.note != "" && !strings.Contains(got, tc.note):
			t.Errorf("%s: notes %q, want %q", tc.name, got, tc.note)
		}
	}
}

// The proxy never carries the version or format: a CLI upgrade would
// change its config hash and recreate it.
func TestProxyHasNoVersionLabels(t *testing.T) {
	l := derive(t, service("api", "registry/app:1", "", true))
	l.inv = invocation{Command: "up"}
	app := l.Derived.Project.Services["api-app"]
	app.Labels = map[string]string{}
	stamp(&app, l.inv)
	if app.Labels[transform.LabelVersion] == "" || app.Labels[transform.LabelFormat] != "1.0" {
		t.Fatalf("replica labels %v", app.Labels)
	}
	for k := range l.Derived.Project.Services["api"].Labels {
		if k == transform.LabelVersion || k == transform.LabelFormat {
			t.Fatalf("proxy has %s", k)
		}
	}
}

func TestMigrateHint(t *testing.T) {
	at := func(f string) map[string]string {
		return map[string]string{transform.LabelFormat: f, transform.LabelVersion: "v0.1.0"}
	}
	for _, tc := range []struct {
		labels []map[string]string
		want   string
	}{
		{[]map[string]string{at("1.0")}, ""},
		{[]map[string]string{at("1.0"), {}}, "project web is at format none, this CLI writes 1.0; run docker bouncer migrate web"},
		{[]map[string]string{at("0.4")}, "project web: format 0.4 is no longer read by this CLI; run docker bouncer down with bouncer v0.1.0, then up with this one"},
	} {
		if got := migrateHint("web", tc.labels); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestMigrateCheck(t *testing.T) {
	at := func(f string) map[string]string {
		return map[string]string{transform.LabelFormat: f, transform.LabelVersion: "v9"}
	}
	for _, tc := range []struct {
		name    string
		labels  []map[string]string
		from    string
		current bool
		err     string
	}{
		{"current", []map[string]string{at("1.0")}, "", true, ""},
		{"missing", []map[string]string{at("1.0"), {}}, "none", false, ""},
		{"newer minor", []map[string]string{at("1.1")}, "", false, "migrate never downgrades"},
		{"unparseable", []map[string]string{at("1.0"), at("x")}, "", false, `format "x" from bouncer v9 is not one this CLI understands`},
		{"unreadable", []map[string]string{at("0.4")}, "", false, "no longer read by this CLI"},
	} {
		from, cur, err := migrateCheck("web", tc.labels)
		if from != tc.from || cur != tc.current || (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %q %v %v", tc.name, from, cur, err)
		}
	}
}

// migrate writes what the chain made of the stored payloads, not the old
// bytes: a fake step from 0.9 rewrites every spec's image and the
// invocation's directory.
func TestMigratedAppStoresMigratedPayloads(t *testing.T) {
	saved := format.Steps
	t.Cleanup(func() { format.Steps = saved })
	format.Steps = []format.Step{{From: format.Format{Minor: 9}, To: format.Current, Fn: func(p format.Payloads) (format.Payloads, error) {
		var out format.Payloads
		for _, s := range p.Specs {
			out.Specs = append(out.Specs, json.RawMessage(strings.ReplaceAll(string(s), "registry/app:1", "registry/app:9")))
		}
		out.Invocation = json.RawMessage(strings.ReplaceAll(string(p.Invocation), "/old", "/new"))
		return out, nil
	}}}
	l := derive(t, service("api", "registry/app:1", "", true))
	l.inv = invocation{Command: "up", Dir: "/cli"}
	app := l.Derived.Project.Services["api-app"]
	cur, _, err := revision.Stamp(app, nil, "u1", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cur, _, err = revision.Stamp(app, cur.Labels, "u2", 10, time.Now()) // a history entry too
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string(cur.Labels)
	labels[transform.LabelFormat] = "0.9"
	labels[transform.LabelInvocation] = invocation{Command: "undo", Revision: 1, Dir: "/old"}.encode()
	got, err := migratedApp(l, l.Derived.Services[0], labels)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels[transform.LabelFormat] != "1.0" || got.Labels[revision.LabelSpecHash] == labels[revision.LabelSpecHash] {
		t.Fatalf("format %q, hash %q (was %q)", got.Labels[transform.LabelFormat], got.Labels[revision.LabelSpecHash], labels[revision.LabelSpecHash])
	}
	h, err := revision.History(got.Labels)
	if err != nil || len(h) != 2 {
		t.Fatalf("history %v %v", h, err)
	}
	for _, e := range h {
		if s, err := revision.Decode(e.Spec); err != nil || s.Image != "registry/app:9" {
			t.Fatalf("revision %d stores image %q (%v), want the migrated one", e.Revision, s.Image, err)
		}
	}
	if inv, _, err := decodeInvocation(got.Labels); err != nil || inv.Dir != "/new" || inv.Command != "undo" {
		t.Fatalf("invocation %+v %v, want the migrated one, still an undo", inv, err)
	}
}

// A malformed invocation label fails the migration instead of being
// replaced by this run's (which would drop an undo hold).
func TestMigratedAppRejectsBadInvocation(t *testing.T) {
	l := derive(t, service("api", "registry/app:1", "", true))
	cur, _, err := revision.Stamp(l.Derived.Project.Services["api-app"], nil, "u1", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string(cur.Labels)
	labels[transform.LabelInvocation] = "not base64!"
	if _, err := migratedApp(l, l.Derived.Services[0], labels); err == nil || !strings.Contains(err.Error(), "invocation label") {
		t.Fatalf("%v, want an invocation error", err)
	}
}

// A migrated revision runs its exact image, and so do the pre_start hooks
// that inherited the service's image, as on undo.
func TestMigratedAppPinsHookImages(t *testing.T) {
	svc := service("api", "registry/app:1", "", true)
	inherited, own := types.PreStartHook{}, types.PreStartHook{}
	inherited.Image, own.Image = "registry/app:1", "registry/tool:1"
	svc.PreStart = []types.PreStartHook{inherited, own}
	l := derive(t, svc)
	app := l.Derived.Project.Services["api-app"]
	app.Labels = types.Labels{labelImage: "sha256:pinned"}
	cur, _, err := revision.Stamp(app, nil, "u1", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := migratedApp(l, l.Derived.Services[0], cur.Labels)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "sha256:pinned" || got.PreStart[0].Image != "sha256:pinned" || got.PreStart[1].Image != "registry/tool:1" {
		t.Fatalf("image %q, hooks %q %q", got.Image, got.PreStart[0].Image, got.PreStart[1].Image)
	}
}
