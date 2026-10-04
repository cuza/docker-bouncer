//go:build e2e

package e2e

import (
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
