package revision

import (
	"bytes"
	"crypto/sha256"
	"fmt"
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
		stamped, _, err := Stamp(a, labels, "up-"+string(rune('0'+i)), 3, now)
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
	s, _, _ := Stamp(app(t, ""), nil, "u", 3, time.Now())
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
	if bytes.Contains(spec, []byte("hunter2")) {
		t.Fatalf("secret in label: %s", spec)
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
	s, _, err := Stamp(app(t, ""), nil, "u1", 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, _, err = Stamp(app(t, ""), s.Labels, "u2", -1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := History(s.Labels); len(h) != 1 {
		t.Fatalf("history %+v", h)
	}
}

func TestStripToleratesRequiredVarSyntax(t *testing.T) {
	a := app(t, "X=${FOO:?must set}\n")
	a.Environment["X"] = strp("set")
	spec, _, err := Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Environment["X"]; ok {
		t.Fatal("env-file key must not be stored")
	}
}

func TestReplicaCountIsNotPartOfTheSpec(t *testing.T) {
	a := app(t, "")
	_, plain, _ := Encode(a)
	a.Deploy = &types.DeployConfig{}
	a.SetScale(2) // `deploy: {replicas: 2}` hashes like no deploy at all
	_, two, _ := Encode(a)
	a.Deploy.Resources.Limits = &types.Resource{MemoryBytes: 1 << 20}
	_, limited, _ := Encode(a)
	a.SetScale(3)
	_, three, err := Encode(a)
	if err != nil || two != plain || three != limited || limited == two {
		t.Fatalf("plain %s two %s limited %s three %s err %v", plain, two, limited, three, err)
	}
	if *a.Deploy.Replicas != 3 {
		t.Fatal("Encode must not mutate the app's Deploy")
	}
}

// stampN stamps n revisions; change(a, i) makes revision i differ.
func stampN(t *testing.T, n, historyMax int, change func(a *types.ServiceConfig, i int)) (map[string]string, int) {
	t.Helper()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var labels map[string]string
	kept := 0
	for i := 1; i <= n; i++ {
		a := app(t, "")
		change(&a, i)
		s, k, err := Stamp(a, labels, fmt.Sprintf("20261003T%06dZ", i), historyMax, now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		labels, kept = s.Labels, k
	}
	return labels, kept
}

// typical: a compose service as the loader hands it over, with ports, a
// healthcheck, labels, a volume and a network.
func typical(a *types.ServiceConfig) {
	a.Command = types.ShellCommand{"serve", "--port", "8080"}
	a.Ports = []types.ServicePortConfig{{Target: 8080, Published: "8080", Protocol: "tcp", Mode: "ingress"}}
	iv, to, retries := types.Duration(10*time.Second), types.Duration(3*time.Second), uint64(3)
	a.HealthCheck = &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "curl", "-f", "http://localhost:8080/health"}, Interval: &iv, Timeout: &to, Retries: &retries}
	a.Labels = types.Labels{"com.example.team": "payments", "traefik.enable": "true", "traefik.http.routers.api.rule": "Host(`api.example.com`)"}
	a.Volumes = []types.ServiceVolumeConfig{{Type: "bind", Source: "/srv/api/data", Target: "/data"}}
	a.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
	a.Restart = "unless-stopped"
	a.Logging = &types.LoggingConfig{Driver: "json-file", Options: types.Options{"max-size": "10m", "max-file": "3"}}
}

func digest(a *types.ServiceConfig, i int) {
	typical(a)
	a.Image = fmt.Sprintf("registry/api@sha256:%064x", uint64(i)*0x9e3779b97f4a7c15)
}

func fortyEnv(a *types.ServiceConfig, i int) {
	typical(a)
	for k := range 40 {
		a.Environment[fmt.Sprintf("SETTING_%02d", k)] = strp(fmt.Sprintf("value-%02d-of-some-typical-length", k))
	}
	a.Environment[fmt.Sprintf("SETTING_%02d", i%40)] = strp(fmt.Sprintf("changed-in-revision-%d", i))
}

// go test -run TestHistorySize -v ./internal/revision prints the size table.
func TestHistorySize(t *testing.T) {
	for _, n := range []int{10, 50, 100, 200} {
		if !testing.Verbose() {
			break // ~700 stamps: slow under -race
		}
		d, _ := stampN(t, n+1, 1000, digest)
		e, _ := stampN(t, n+1, 1000, fortyEnv)
		t.Logf("%d revisions: digest-only %d B, 40 env %d B", n, len(d[LabelHistory]), len(e[LabelHistory]))
	}
	labels, kept := stampN(t, 101, 100, digest)
	h, err := History(labels)
	if err != nil || len(h) != 101 || kept != 0 {
		t.Fatalf("history %d, kept %d, err %v", len(h), kept, err)
	}
	if n := len(labels[LabelHistory]); n > 16<<10 {
		t.Fatalf("100 typical revisions take %d B, want < 16 KiB", n)
	}
	if _, err := Decode(h[100].Spec); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryBudgetTrims(t *testing.T) {
	// Incompressible env values: every revision adds ~3 KiB.
	labels, kept := stampN(t, 60, 1000, func(a *types.ServiceConfig, i int) {
		var noise strings.Builder
		for j := range 64 {
			fmt.Fprintf(&noise, "%x", sha256.Sum256([]byte{byte(i), byte(j)}))
		}
		a.Environment["NOISE"] = strp(noise.String())
	})
	h, err := History(labels)
	if err != nil {
		t.Fatal(err)
	}
	if kept == 0 || kept != len(h) || len(h) >= 60 || len(labels[LabelHistory]) > HistoryBudget {
		t.Fatalf("kept %d, history %d, label %d B", kept, len(h), len(labels[LabelHistory]))
	}
	if h[0].Revision != 60 || h[len(h)-1].Revision != 60-len(h)+1 {
		t.Fatalf("must drop the oldest: %d..%d", h[0].Revision, h[len(h)-1].Revision)
	}
}
