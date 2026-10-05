//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A stack written by a newer major format is refused by up before anything
// changes; read-only commands still work, with a warning.
func TestUnsupportedFormatRefused(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast))
	newer := p.cmd("up")
	newer.Env = append(os.Environ(), "BOUNCER_E2E_FORMAT=2.0")
	if out, err := newer.CombinedOutput(); err != nil {
		t.Fatalf("up as format 2.0: %v\n%s", err, out)
	}
	before := p.ids()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	out, code := p.bouncer("up")
	if code != 2 || !strings.Contains(out, "format 2.0 from bouncer dev is newer than this CLI (dev) can safely change; upgrade docker-bouncer, or pass --ignore-format") {
		t.Fatalf("up: %d\n%s", code, out)
	}
	if after := p.ids(); len(after) != len(before) {
		t.Fatalf("a refused up changed containers: %v → %v", before, after)
	}
	for k, v := range before {
		if p.ids()[k] != v {
			t.Fatalf("a refused up recreated %s", k)
		}
	}
	if out, code := p.bouncer("ps"); code != 0 || !strings.Contains(out, "is newer than this CLI") {
		t.Fatalf("ps: %d\n%s", code, out)
	}
	// refresh fails the project (exit 1, not a usage error) ...
	if out, code := p.refresh("", "--pull", "never"); code != 1 || !strings.Contains(out, "Failed: project "+p.name+": format 2.0") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	// ... but a stopped project is skipped before its format matters.
	if out, code := p.bouncer("stop"); code != 0 {
		t.Fatalf("stop: %d\n%s", code, out)
	}
	if out, code := p.refresh("", "--pull", "never"); code != 0 || !strings.Contains(out, "Skipped: stopped") {
		t.Fatalf("refresh of a stopped project: %d\n%s", code, out)
	}
	out, _ = p.bouncer("ls")
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 6 && f[0] == p.name && f[6] != "2.0" {
			t.Fatalf("ls FORMAT: %q", line)
		}
	}
}

// A stack from before formats were recorded: ls asks to migrate it, a dry
// run changes nothing, migrate re-bounces it with no failed request, and a
// second migrate (or an up) finds nothing to do.
func TestMigrate(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: {{APP}}:v1
    deploy: { replicas: 2 }
    ports: ["127.0.0.1:%d:8080"]
    healthcheck: { test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"], interval: 1s }
    x-bouncer: { %s, drain_method_params: { delay: 2s } }
`, port, fast))
	old := p.cmd("up")
	old.Env = append(os.Environ(), "BOUNCER_E2E_FORMAT=none")
	if out, err := old.CombinedOutput(); err != nil {
		t.Fatalf("up without a format: %v\n%s", err, out)
	}
	hint := "project " + p.name + " is at format none, this CLI writes 1.0; run docker bouncer migrate " + p.name
	if out, _ := p.bouncer("ls"); !strings.Contains(out, hint) {
		t.Fatalf("ls: no hint\n%s", out)
	}
	before := p.ids()
	out, code := p.bouncer("migrate", "--dry-run")
	if code != 0 || !strings.Contains(out, "Would migrate: none → 1.0; would bounce api") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	if after := p.ids(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("dry run changed containers %v → %v", before, after)
	}
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	out, code = p.bouncer("migrate")
	close(stop)
	if code != 0 || !strings.Contains(out, "Migrated to format 1.0") {
		t.Fatalf("migrate: %d\n%s", code, out)
	}
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	reps := p.replicas("api")
	for _, id := range reps {
		if f := docker(t, "inspect", "-f", `{{index .Config.Labels "dev.cuza.bouncer.format"}}`, id); f != "1.0" {
			t.Fatalf("replica %s format %q after migrate", id, f)
		}
	}
	if len(reps) != 2 || len(p.history("api")) != 1 {
		t.Fatalf("replicas %v, history %q: migrate keeps the revision", reps, p.history("api"))
	}
	after := p.ids()
	if out, code := p.bouncer("migrate"); code != 0 || !strings.Contains(out, "Up to date") {
		t.Fatalf("second migrate: %d\n%s", code, out)
	}
	p.mustUp()
	if again := p.ids(); fmt.Sprint(again) != fmt.Sprint(after) {
		t.Fatalf("migrate again or up changed containers %v → %v", after, again)
	}
}

// A stopped project has no running replica to bounce from: migrate skips it.
func TestMigrateSkipsStopped(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast))
	old := p.cmd("up")
	old.Env = append(os.Environ(), "BOUNCER_E2E_FORMAT=none")
	if out, err := old.CombinedOutput(); err != nil {
		t.Fatalf("up without a format: %v\n%s", err, out)
	}
	if out, code := p.bouncer("stop"); code != 0 {
		t.Fatalf("stop: %d\n%s", code, out)
	}
	if out, code := p.bouncer("migrate"); code != 0 || !strings.Contains(out, "Skipped: stopped") || !strings.Contains(out, "1 skipped") {
		t.Fatalf("migrate: %d\n%s", code, out)
	}
}

// The format is checked again under the lock: here another run rewrote the
// stack at format 2.0 while this up waited for the lock.
func TestFormatRecheckedUnderLock(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast))
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	slow := p.cmd("up")
	slow.Env = append(os.Environ(), "BOUNCER_E2E_LOCK_DELAY=40s")
	var buf strings.Builder
	slow.Stdout, slow.Stderr = &buf, &buf
	if err := slow.Start(); err != nil {
		t.Fatal(err)
	}
	newer := p.cmd("up")
	newer.Env = append(os.Environ(), "BOUNCER_E2E_FORMAT=2.0")
	if out, err := newer.CombinedOutput(); err != nil {
		t.Fatalf("up as format 2.0: %v\n%s", err, out)
	}
	err := slow.Wait()
	if code := slow.ProcessState.ExitCode(); code != 2 || !strings.Contains(buf.String(), "format 2.0 from bouncer dev is newer than this CLI") {
		t.Fatalf("the waiting up: exit %d (%v)\n%s", code, err, buf.String())
	}
}
