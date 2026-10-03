package cli

import (
	"bytes"
	"testing"
)

func TestVersion(t *testing.T) {
	old := Version
	Version = "v9.9.9"
	defer func() { Version = old }()
	for _, args := range [][]string{{"version"}, {"--version"}} {
		root := NewRoot(nil)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := out.String(); got != "docker bouncer v9.9.9\n" {
			t.Fatalf("%v: got %q", args, got)
		}
	}
}
