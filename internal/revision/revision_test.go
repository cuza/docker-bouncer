package revision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func strp(s string) *string { return &s }

func app(t *testing.T, envFileContent string) types.ServiceConfig {
	t.Helper()
	f := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(f, []byte(envFileContent), 0o600); err != nil {
		t.Fatal(err)
	}
	a := types.ServiceConfig{Name: "api-app"}
	a.Image = "registry/api@sha256:1"
	a.EnvFiles = []types.EnvFile{{Path: f}}
	a.Environment = types.MappingWithEquals{"PASSWORD": strp("old"), "PORT": strp("8080")}
	return a
}

func TestStripRemovesEnvFileValuesOnly(t *testing.T) {
	s, err := Strip(app(t, "PASSWORD=old\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Environment["PASSWORD"]; ok {
		t.Fatal("env-file secret must not be stored")
	}
	if *s.Environment["PORT"] != "8080" {
		t.Fatal("explicit environment must stay")
	}
}

func TestSpecHashStable(t *testing.T) {
	a := app(t, "PASSWORD=old\n")
	_, h1, _ := Encode(a)
	_, h2, _ := Encode(a)
	if h1 != h2 || h1 == "" {
		t.Fatal("hash not deterministic")
	}
	b := a
	b.Image = "registry/api@sha256:2"
	if _, h3, _ := Encode(b); h3 == h1 {
		t.Fatal("image change must change the hash")
	}
}

func TestRestoreUsesCurrentSecrets(t *testing.T) {
	a := app(t, "PASSWORD=old\n")
	spec, _, _ := Encode(a)
	decoded, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(a.EnvFiles[0].Path, []byte("PASSWORD=rotated\n"), 0o600)
	r, err := Restore(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if *r.Environment["PASSWORD"] != "rotated" || *r.Environment["PORT"] != "8080" {
		t.Fatalf("env %v", r.Environment)
	}
}

func TestStampBuildsHistoryAndTruncates(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var labels map[string]string
	for i := 1; i <= 5; i++ {
		a := app(t, "")
		a.Image = "registry/api@sha256:" + strings.Repeat("a", i)
		stamped, err := Stamp(a, labels, "up-"+string(rune('0'+i)), 3, now)
		if err != nil {
			t.Fatal(err)
		}
		labels = stamped.Labels
	}
	h, err := History(labels)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 || h[0].Revision != 5 || h[1].Revision != 4 || h[3].Revision != 2 {
		t.Fatalf("history %+v", h)
	}
	if labels[LabelUpID] != "up-5" {
		t.Fatal("up-id")
	}
}

func TestStampRevisionStartsAtOne(t *testing.T) {
	s, _ := Stamp(app(t, ""), nil, "u", 3, time.Now())
	if s.Labels[LabelRevision] != "1" {
		t.Fatal(s.Labels)
	}
}

func TestLabelChangeChangesHash(t *testing.T) {
	a := app(t, "")
	a.Labels = types.Labels{"team": "a"}
	_, h1, _ := Encode(a)
	a.Labels = types.Labels{"team": "b"}
	_, h2, _ := Encode(a)
	if h1 == h2 {
		t.Fatal("user labels are part of the spec")
	}
	a.Labels[LabelRevision] = "9"
	if _, h3, _ := Encode(a); h3 != h2 {
		t.Fatal("revision labels must not affect the hash")
	}
}
