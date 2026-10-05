//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// refresh runs `docker bouncer refresh <project> args...` from dir (the
// project's own when ""); never without the project, which would refresh
// every project on the host.
func (p *proj) refresh(dir string, args ...string) (string, int) {
	p.t.Helper()
	c := exec.Command("docker", append([]string{"bouncer", "refresh", p.name}, args...)...)
	c.Dir = p.dir
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	case err != nil:
		p.t.Fatalf("docker bouncer refresh: %v", err)
	}
	return string(out), 0
}

// movingTag tags image as a tag unique to the test, removed at the end.
func movingTag(t *testing.T, name, image string) string {
	t.Helper()
	tag := app(fmt.Sprintf("%s-%d", name, projectSeq.Add(1)))
	docker(t, "tag", image, tag)
	t.Cleanup(func() { exec.Command("docker", "rmi", tag).Run() })
	return tag
}

// The Watchtower case: the tag moved, refresh bounces with no failed
// request, and a second refresh finds nothing to do.
func TestRefreshMovesTag(t *testing.T) {
	port := freePort(t)
	tag := movingTag(t, "moving", app("v1"))
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: %s
    deploy: { replicas: 2 }
    ports: ["127.0.0.1:%d:8080"]
    healthcheck: { test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"], interval: 1s }
    x-bouncer: { %s, drain_method_params: { delay: 5s } }
`, tag, port, fast))
	p.mustUp()
	docker(t, "tag", app("v2"), tag)
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	out, code := p.refresh(t.TempDir(), "--pull", "never")
	close(stop)
	if code != 0 || !strings.Contains(out, "Refreshed") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v2") {
		t.Fatalf("after refresh GET / = %q, want v2", body)
	}
	before := p.replicas("api")
	out, code = p.refresh("", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Up to date") || strings.Contains(out, "Bouncing") {
		t.Fatalf("second refresh: %d\n%s", code, out)
	}
	if after := p.replicas("api"); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Fatalf("second refresh changed replicas %v → %v", before, after)
	}
	if h := p.history("api"); len(h) != 2 {
		t.Fatalf("history: %q", h)
	}
}

func TestRefreshAllRecreatesPlain(t *testing.T) {
	port := freePort(t)
	tag := movingTag(t, "plain", app("v1"))
	p := project(t, api(port, 1, fast)+fmt.Sprintf(`
  worker:
    image: %s
`, tag))
	p.mustUp()
	worker := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker")
	// Compose's config hashes agree with the dry run's: nothing to do yet.
	if out, code := p.refresh("", "--dry-run", "-a", "--pull", "never"); code != 0 || strings.Contains(out, "Would") || !strings.Contains(out, "1 up to date") {
		t.Fatalf("dry run -a after up: %d\n%s", code, out)
	}
	docker(t, "tag", app("v2"), tag)
	if out, code := p.refresh("", "--dry-run", "--pull", "never"); code != 0 || !strings.Contains(out, "Service worker Would recreate with -a: image "+tag+" moved") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	out, code := p.refresh("", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Up to date") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if got := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker"); strings.Join(got, " ") != strings.Join(worker, " ") {
		t.Fatalf("without -a the plain service changed: %v → %v\n%s", worker, got, out)
	}
	out, code = p.refresh("", "-a", "--pull", "never")
	got := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker")
	if code != 0 || !strings.Contains(out, "Refreshed") || len(got) != 1 || got[0] == worker[0] {
		t.Fatalf("refresh -a: %d, worker %v → %v\n%s", code, worker, got, out)
	}
	if img, v2 := docker(t, "inspect", "-f", "{{.Image}}", got[0]), docker(t, "image", "inspect", "-f", "{{.Id}}", app("v2")); img != v2 {
		t.Fatalf("worker runs %s, want v2 %s", img, v2)
	}
}

func TestRefreshSkipsLocked(t *testing.T) {
	tag := movingTag(t, "locked", app("v1"))
	p := project(t, strings.Replace(api(freePort(t), 1, fast), "{{APP}}:v1", tag, 1))
	p.mustUp()
	docker(t, "create", "--name", p.name+"-bouncer-lock", "--label", "dev.cuza.bouncer.role=lock",
		"--label", "dev.cuza.bouncer.lock-owner=someone-else", "--label", "com.docker.compose.project="+p.name,
		"envoyproxy/envoy:v1.39.1", "true")
	docker(t, "tag", app("v2"), tag)
	before := p.replicas("api")
	// Pulling is allowed: the local-only tag would fail to pull, so this also
	// shows the lock is checked before the pull.
	out, code := p.refresh("")
	if code != 0 || !strings.Contains(out, "Skipped: bounce in progress by someone-else") || !strings.Contains(out, "1 skipped") || strings.Contains(out, "Pull") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if after := p.replicas("api"); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Fatalf("a locked project bounced: %v → %v", before, after)
	}
}

// up from one directory with an env file and a profile; refresh from
// another loads the same project: the env file's tag, the profiled services.
func TestRefreshReplaysEnvFileAndProfiles(t *testing.T) {
	port := freePort(t)
	tag := movingTag(t, "env", app("v1"))
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: {{APP}}:${TAG}
    profiles: [p]
    deploy: { replicas: 1 }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s }
  worker:
    image: {{APP}}:v1
    profiles: [p]
`, port, fast))
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "x.env"), []byte("TAG="+strings.TrimPrefix(tag, appImage+":")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := exec.Command("docker", "bouncer", "-p", p.name, "-f", filepath.Join(p.dir, "compose.yaml"), "--env-file", "x.env", "--profile", "p", "up")
	up.Dir = cwd
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	t.Cleanup(func() { // the profiled services need the profile to go down
		down := exec.Command("docker", "bouncer", "-p", p.name, "-f", filepath.Join(p.dir, "compose.yaml"), "--env-file", "x.env", "--profile", "p", "down")
		down.Dir = cwd
		down.Run()
	})
	worker := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker")
	docker(t, "tag", app("v2"), tag)
	out, code := p.refresh(t.TempDir(), "-a", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Refreshed") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v2") {
		t.Fatalf("after refresh GET / = %q, want v2", body)
	}
	if img := p.images("api"); img[tag] != 1 || len(img) != 1 {
		t.Fatalf("replicas run %v, want %s", img, tag)
	}
	if got := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker"); len(worker) != 1 || strings.Join(got, " ") != worker[0] {
		t.Fatalf("worker %v → %v\n%s", worker, got, out)
	}
}

func TestRefreshJSON(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast))
	p.mustUp()
	out, code := p.refresh("", "--progress", "json", "--pull", "never")
	if code != 0 {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, line := range lines {
		var e struct{ ID, Project, Message string }
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not JSON: %q: %v\n%s", line, err, out)
		}
		last := i == len(lines)-1
		switch {
		case last && (e.ID != "Refresh" || e.Message != "1 project: 1 up to date"):
			t.Fatalf("summary: %q", line)
		case !last && e.Project != p.name:
			t.Fatalf("no project: %q", line)
		}
	}
}

func TestRefreshDryRun(t *testing.T) {
	port := freePort(t)
	tag := movingTag(t, "dry", app("v1"))
	p := project(t, strings.Replace(api(port, 1, fast), "{{APP}}:v1", tag, 1))
	p.mustUp()
	docker(t, "tag", app("v2"), tag)
	before := p.ids()
	out, code := p.refresh("", "--dry-run", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Would bounce: image "+tag+" moved from ") || !strings.Contains(out, "1 would change") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	if after := p.ids(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("dry run changed containers %v → %v", before, after)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("after dry run GET / = %q, want v1", body)
	}
}

// After undo the file still describes the newer revision: refresh holds the
// Service where undo put it until the next up.
func TestRefreshRespectsUndo(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, drain_method_params: { delay: 1s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	p.mustUp()
	if out, code := p.bouncer("undo"); code != 0 {
		t.Fatalf("undo: %d\n%s", code, out)
	}
	out, code := p.refresh("", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Held at revision 1 by undo") {
		t.Fatalf("refresh after undo: %d\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("after refresh GET / = %q, want v1 (held)", body)
	}
	p.mustUp()
	out, code = p.refresh("", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Up to date") || strings.Contains(out, "Held") {
		t.Fatalf("refresh after up: %d\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v2") {
		t.Fatalf("after up GET / = %q, want v2", body)
	}
}

// An up that only scales records its own invocation on the replicas it
// adds, and refresh replays the newest one.
func TestRefreshReplaysNewestInvocation(t *testing.T) {
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: {{APP}}:v1
    deploy: { replicas: "${N:-1}" }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s }
`, freePort(t), fast))
	for _, f := range []string{"a.env", "b.env"} {
		n := map[string]string{"a.env": "1", "b.env": "2"}[f]
		if err := os.WriteFile(filepath.Join(p.dir, f), []byte("N="+n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a.env", "b.env"} {
		if out, code := p.bouncer("--env-file", f, "up"); code != 0 {
			t.Fatalf("up --env-file %s: %d\n%s", f, code, out)
		}
	}
	t.Cleanup(func() {
		exec.Command("docker", "bouncer", "-p", p.name, "--env-file", filepath.Join(p.dir, "b.env"), "-f", filepath.Join(p.dir, "compose.yaml"), "down").Run()
	})
	if h := p.history("api"); len(h) != 1 {
		t.Fatalf("scaling made a revision: %q", h)
	}
	os.Remove(filepath.Join(p.dir, "a.env")) // replaying the first up would fail
	out, code := p.refresh(t.TempDir(), "--pull", "never")
	if code != 0 || !strings.Contains(out, "Up to date") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if n := len(p.replicas("api")); n != 2 {
		t.Fatalf("%d replicas after refresh, want 2", n)
	}
}

// A replica whose invocation loads another project fails its project
// before anything is locked, pulled or bounced.
func TestRefreshInvocationOfAnotherProject(t *testing.T) {
	other := project(t, api(freePort(t), 1, fast))
	p := project(t, api(freePort(t), 1, fast))
	j, _ := json.Marshal(map[string]any{"version": "dev", "command": "up", "files": []string{filepath.Join(other.dir, "compose.yaml")}, "dir": other.dir, "name": other.name})
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(j)
	zw.Close()
	docker(t, "run", "-d", "--label", "com.docker.compose.project="+p.name, "--label", "dev.cuza.bouncer.role=replica",
		"--label", "dev.cuza.bouncer.service=api", "--label", "com.docker.compose.service=api-app", "--label", "dev.cuza.bouncer.up-id=20260101T000000Z",
		"--label", "dev.cuza.bouncer.invocation="+base64.StdEncoding.EncodeToString(buf.Bytes()), app("v1"))
	out, code := p.refresh("", "--pull", "never")
	if code != 1 || !strings.Contains(out, "the recorded invocation loads project "+other.name+", not "+p.name) || strings.Contains(out, "Lock") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if ids := other.containers("com.docker.compose.project=" + other.name); len(ids) != 0 {
		t.Fatalf("refresh touched the other project: %v", ids)
	}
}

// refresh -a starting a stopped plain service is a change, not "Up to date".
func TestRefreshAllStartsStoppedPlain(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast)+`
  worker:
    image: {{APP}}:v1
`)
	p.mustUp()
	worker := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=worker")
	docker(t, "stop", worker[0])
	out, code := p.refresh("", "-a", "--pull", "never")
	if code != 0 || !strings.Contains(out, "Refreshed") {
		t.Fatalf("refresh -a: %d\n%s", code, out)
	}
	if state := docker(t, "inspect", "-f", "{{.State.Status}}", worker[0]); state != "running" {
		t.Fatalf("worker %s after refresh -a", state)
	}
}
