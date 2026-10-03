package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cuza/docker-bouncer/internal/config"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRoot(nil)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

func TestVersion(t *testing.T) {
	old := Version
	Version = "v9.9.9"
	defer func() { Version = old }()

	if got := run(t, "--version"); got != "docker bouncer v9.9.9\n" {
		t.Fatalf("--version: got %q", got)
	}
	if got := run(t, "version", "--short"); got != "v9.9.9\n" {
		t.Fatalf("version --short: got %q", got)
	}
	got := run(t, "version")
	for _, want := range []string{"docker bouncer v9.9.9\n", "  compose: ", "  envoy:   " + config.DefaultProxyImage} {
		if !strings.Contains(got, want) {
			t.Fatalf("version: %q lacks %q", got, want)
		}
	}
}
