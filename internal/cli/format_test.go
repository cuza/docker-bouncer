package cli

import (
	"strings"
	"testing"

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
		{"missing", []map[string]string{at("", "")}, true, false, "", "deployed before bouncer recorded its version", true},
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
