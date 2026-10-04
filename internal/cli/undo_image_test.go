package cli

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestUndoImageCheck(t *testing.T) {
	svc := func(img, policy string) types.ServiceConfig {
		s := types.ServiceConfig{Name: "api-app"}
		s.PullPolicy = policy
		s.Image = img
		return s
	}
	digest := "registry/app@sha256:" + strings.Repeat("a", 64)
	id := "sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name    string
		run     types.ServiceConfig
		present bool
		want    string // "" = no error
	}{
		{"local image id present", svc(id, ""), true, ""},
		{"local image id gone", svc(id, ""), false, "no longer local"},
		{"digest missing, pulls allowed", svc(digest, ""), false, ""},
		{"digest missing, pull_policy never", svc(digest, types.PullPolicyNever), false, "pull_policy is never"},
		{"digest missing, pull_policy build", svc(digest, types.PullPolicyBuild), false, "pull_policy is build"},
		{"digest present, pull_policy never", svc(digest, types.PullPolicyNever), true, ""},
	}
	for _, c := range cases {
		err := undoImageCheck("api", 3, "registry/app:v1", c.run, c.present)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}
