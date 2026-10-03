package revision

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func strp(s string) *string { return &s }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func app(t *testing.T, envFileContent string) types.ServiceConfig {
	t.Helper()
	f := filepath.Join(t.TempDir(), "app.env")
	write(t, f, envFileContent)
	a := types.ServiceConfig{Name: "api-app"}
	a.Image = "registry/api@sha256:1"
	a.EnvFiles = []types.EnvFile{{Path: f}}
	a.Environment = types.MappingWithEquals{"PASSWORD": strp("old"), "PORT": strp("8080")}
	return a
}

func TestStripRemovesEnvFileKeysOnly(t *testing.T) {
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
	write(t, a.EnvFiles[0].Path, "PASSWORD=rotated\n")
	r, err := Restore(decoded, nil)
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

func TestStripIsKeyBasedForInterpolatedSecrets(t *testing.T) {
	a := app(t, "DB_URL=postgres://u:${DB_PASS}@h\n")
	a.Environment["DB_URL"] = strp("postgres://u:hunter2@h") // as the loader resolved it
	spec, _, err := Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Environment["DB_URL"]; ok {
		t.Fatal("env-file key must not be stored")
	}
	var raw json.RawMessage
	if err := unpack(spec, &raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("secret in label: %s", raw)
	}
}

func TestDecodeKeepsRequired(t *testing.T) {
	a := app(t, "PASSWORD=old\n")
	a.EnvFiles[0].Required = true
	spec, _, err := Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bool(d.EnvFiles[0].Required) {
		t.Fatal("required lost")
	}
	if err := os.Remove(a.EnvFiles[0].Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(d, nil); err == nil {
		t.Fatal("missing required env file must fail")
	}
}

func TestRestoreInterpolatesProjectEnv(t *testing.T) {
	a := app(t, "DB_URL=postgres://u:${DB_PASS}@h\n")
	a.Environment["DB_URL"] = strp("postgres://u:hunter2@h")
	spec, _, _ := Encode(a)
	d, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Restore(d, map[string]string{"DB_PASS": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if got := *r.Environment["DB_URL"]; got != "postgres://u:new@h" {
		t.Fatal(got)
	}
}

func TestRestoreRawFormatVerbatim(t *testing.T) {
	a := app(t, "TOKEN=a$b${c}\n")
	a.EnvFiles[0].Format = "raw"
	a.Environment["TOKEN"] = strp("a$b${c}")
	spec, _, err := Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Environment["TOKEN"]; ok {
		t.Fatal("raw env-file key must not be stored")
	}
	r, err := Restore(d, map[string]string{"c": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := *r.Environment["TOKEN"]; got != "a$b${c}" {
		t.Fatal(got)
	}
}

func TestStampNegativeHistoryMax(t *testing.T) {
	s, err := Stamp(app(t, ""), nil, "u1", 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err = Stamp(app(t, ""), s.Labels, "u2", -1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := History(s.Labels); len(h) != 1 {
		t.Fatalf("history %+v", h)
	}
}
