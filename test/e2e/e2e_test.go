//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fast keeps bounces short: the scenarios are about ordering, not timing.
const fast = `min_task_uptime: 1s, bounce_health_timeout: 60s`

func api(port, replicas int, xb string) string {
	return fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    deploy: { replicas: %d }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s }
`, replicas, port, xb)
}

func url(port int, path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", port, path) }

// images maps each replica's image to its count.
func (p *proj) images(svc string) map[string]int {
	out := map[string]int{}
	for _, id := range p.replicas(svc) {
		out[docker(p.t, "inspect", "-f", "{{.Config.Image}}", id)]++
	}
	return out
}

// history returns the rows of `history svc` (header dropped).
func (p *proj) history(svc string) []string {
	p.t.Helper()
	out, code := p.bouncer("history", svc)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || !strings.HasPrefix(lines[0], "REVISION") {
		p.t.Fatalf("history %s: %d\n%s", svc, code, out)
	}
	return lines[1:]
}

// ids is every container of the project, name → ID.
func (p *proj) ids() map[string]string {
	out := map[string]string{}
	for _, id := range p.containers("com.docker.compose.project=" + p.name) {
		out[docker(p.t, "inspect", "-f", "{{.Name}}", id)] = id
	}
	return out
}

func TestFirstDeploy(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast))
	p.mustUp()
	out, code := p.bouncer("ps")
	if code != 0 {
		t.Fatalf("ps: %d\n%s", code, out)
	}
	roles := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		roles[strings.Fields(l)[2]]++
	}
	if roles["proxy"] != 1 || roles["live"] != 2 || len(roles) != 2 {
		t.Fatalf("ps roles %v\n%s", roles, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("GET / = %q", body)
	}
	// The proxy routes by the cluster file Bouncer wrote, not the seed alias.
	var want, got []string
	for _, id := range p.replicas("api") {
		want = append(want, strings.TrimPrefix(docker(t, "inspect", "-f", "{{.Name}}", id), "/"))
	}
	for _, l := range strings.Split(p.proxyClusters("api", 9901), "\n") {
		if f := strings.Split(strings.TrimSpace(l), "::"); len(f) == 4 && f[2] == "hostname" {
			got = append(got, f[3])
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("/clusters hostnames %v, want the replicas %v", got, want)
	}
}

func TestCrossoverUnderLoadZeroErrors(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    deploy: { replicas: 2 }
    ports: ["127.0.0.1:%d:8080"]
    healthcheck: { test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"], interval: 1s }
    x-bouncer: { %s, drain_method_params: { delay: 5s } }
`, port, fast))
	p.mustUp()
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("bounce: %d\n%s", code, out)
	}
	close(stop)
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	if img := p.images("api"); img["bouncer-e2e-app:v2"] != 2 || len(img) != 1 {
		t.Fatalf("replica images %v", img)
	}
	if h := p.history("api"); len(h) != 2 {
		t.Fatalf("history: %q", h)
	}
}

func TestSlowRequestsSurviveBounce(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast+`, drain_method_params: { delay: 30s }`))
	p.mustUp()
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range results {
		wg.Go(func() {
			code, body, err := get(url(port, "/slow?s=8"))
			results[i] = fmt.Sprintf("%d %s %v", code, strings.TrimSpace(body), err)
		})
	}
	time.Sleep(time.Second) // in flight before the bounce starts
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("bounce: %d\n%s", code, out)
	}
	wg.Wait()
	for _, r := range results {
		if !strings.HasPrefix(r, "200 ") || !strings.HasSuffix(r, "v1 <nil>") {
			t.Fatalf("slow requests: %q", results)
		}
	}
}

func TestNeverHealthyFailsAndOldServes(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, `min_task_uptime: 1s, bounce_health_timeout: 15s, drain_method_params: { delay: 2s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, "image: bouncer-e2e-app:v1", "image: bouncer-e2e-app:v2\n    environment: { UNHEALTHY: \"1\" }", 1))
	if out, code := p.bouncer("up"); code != 1 {
		t.Fatalf("bounce of a never-healthy version: exit %d, want 1\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("GET / = %q", body)
	}
	if img := p.images("api"); img["bouncer-e2e-app:v1"] != 2 || len(img) != 1 {
		t.Fatalf("replica images after the failed bounce %v (no v2 may remain)", img)
	}
}

func TestUpTwiceIsNoop(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast)+`
  side:
    image: alpine:3.22
    command: ["sleep", "infinity"]
    stop_signal: SIGKILL
`)
	p.mustUp()
	before := p.ids()
	out, code := p.bouncer("up")
	if code != 0 {
		t.Fatalf("second up: %d\n%s", code, out)
	}
	if strings.Contains(out, "proxy definition changed") {
		t.Fatalf("second up warned about the proxy:\n%s", out)
	}
	if after := p.ids(); !mapsEqual(before, after) {
		t.Fatalf("container IDs changed:\nbefore %v\nafter  %v", before, after)
	}
	if h := p.history("api"); len(h) != 1 {
		t.Fatalf("history: %q", h)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestReplicaChangeScalesWithoutBounce(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast+`, drain_method_params: { delay: 2s }`))
	p.mustUp()
	orig := p.replicas("api")
	p.write(strings.Replace(p.yaml, "replicas: 2", "replicas: 3", 1))
	p.mustUp()
	now := p.replicas("api")
	if len(now) != 3 {
		t.Fatalf("%d replicas after scaling to 3", len(now))
	}
	for _, id := range orig {
		if !slices.Contains(now, id) {
			t.Fatalf("replica %s was replaced by a scale-up", id)
		}
	}
	for _, id := range now {
		if rev := docker(t, "inspect", "-f", `{{index .Config.Labels "bouncer.revision"}}`, id); rev != "1" {
			t.Fatalf("replica %s has revision %s, want 1", id, rev)
		}
	}
	if h := p.history("api"); len(h) != 1 {
		t.Fatalf("history after scaling: %q", h)
	}
	p.write(strings.Replace(p.yaml, "replicas: 3", "replicas: 1", 1))
	p.mustUp()
	if now := p.replicas("api"); len(now) != 1 {
		t.Fatalf("%d replicas after scaling to 1", len(now))
	}
	mustGet(t, url(port, "/"))
}

// bg is a plugin run in the background.
type bg struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  strings.Builder
	done chan struct{}
	code int
}

func (b *bg) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *bg) output() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String()
}

func (p *proj) start(args ...string) *bg {
	p.t.Helper()
	b := &bg{cmd: p.cmd(args...), done: make(chan struct{})}
	b.cmd.Stdout, b.cmd.Stderr = b, b
	if err := b.cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	go func() {
		b.cmd.Wait()
		b.code = b.cmd.ProcessState.ExitCode()
		close(b.done)
	}()
	p.t.Cleanup(func() { b.cmd.Process.Kill(); <-b.done })
	return b
}

// waitFor blocks until the output contains s.
func (b *bg) waitFor(t *testing.T, s string, timeout time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(timeout); !strings.Contains(b.output(), s); {
		select {
		case <-b.done:
			t.Fatalf("exited %d before %q:\n%s", b.code, s, b.output())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q within %v:\n%s", s, timeout, b.output())
		}
	}
}

func (b *bg) wait(t *testing.T, timeout time.Duration) (string, int) {
	t.Helper()
	select {
	case <-b.done:
		return b.output(), b.code
	case <-time.After(timeout):
		t.Fatalf("still running after %v:\n%s", timeout, b.output())
	}
	return "", 0
}

// plugin is the docker-bouncer process the docker CLI started.
func (b *bg) plugin(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", fmt.Sprint(b.cmd.Process.Pid)).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) != 1 {
		t.Fatalf("plugin child of %d: %q %v", b.cmd.Process.Pid, out, err)
	}
	var pid int
	fmt.Sscan(f[0], &pid)
	return pid
}

func TestResumeAfterKill(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast+`, drain_method_params: { delay: 40s }`))
	p.mustUp()
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	slow := make(chan string, 2)
	for range 2 { // one per replica: whichever drains first stays busy
		go func() { code, body, err := get(url(port, "/slow?s=20")); slow <- fmt.Sprint(code, body, err) }()
	}
	time.Sleep(time.Second)
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	b := p.start("up")
	b.waitFor(t, "draining", 2*time.Minute)
	time.Sleep(time.Second)
	if err := syscall.Kill(b.plugin(t), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	b.wait(t, 10*time.Second)
	if out, code := p.bouncer("up", "--force-unlock"); code != 0 {
		t.Fatalf("resumed up: %d\n%s", code, out)
	}
	close(stop)
	if r := <-res; r.failures != 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	for range 2 {
		if s := <-slow; !strings.HasPrefix(s, "200") {
			t.Fatalf("slow request: %s", s)
		}
	}
	if img := p.images("api"); img["bouncer-e2e-app:v2"] != 2 || len(img) != 1 {
		t.Fatalf("replica images %v", img)
	}
}

func TestInterruptReleasesLock(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, drain_method_params: { delay: 60s }`))
	p.mustUp()
	go get(url(port, "/slow?s=30")) // keeps the old replica draining
	time.Sleep(time.Second)
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	b := p.start("up")
	b.waitFor(t, "draining", 2*time.Minute)
	// What Ctrl-C on a TTY delivers to the plugin (the CLI leaves it to the kernel).
	if err := syscall.Kill(b.plugin(t), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if out, code := b.wait(t, 15*time.Second); code == 0 {
		t.Fatalf("interrupted up exited 0:\n%s", out)
	}
	if ids := p.containers("bouncer.role=lock", "com.docker.compose.project="+p.name); len(ids) != 0 {
		t.Fatalf("lock container left behind: %v", ids)
	}
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("up after interrupt: %d\n%s", code, out)
	}
	if img := p.images("api"); img["bouncer-e2e-app:v2"] != 1 || len(img) != 1 {
		t.Fatalf("replica images %v", img)
	}
}

func TestConcurrentUpHitsLock(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, drain_method_params: { delay: 2s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	a, b := p.start("up"), p.start("up")
	outA, codeA := a.wait(t, 2*time.Minute)
	outB, codeB := b.wait(t, 2*time.Minute)
	if codeA == codeB {
		t.Fatalf("both exited %d\n--- a\n%s\n--- b\n%s", codeA, outA, outB)
	}
	loser := outA
	if codeB != 0 {
		loser = outB
	}
	if codeA+codeB != 1 || !strings.Contains(loser, "bounce in progress by") {
		t.Fatalf("exits %d/%d\n--- a\n%s\n--- b\n%s", codeA, codeB, outA, outB)
	}
}

func TestUndoAndUndoOfUndo(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, drain_method_params: { delay: 2s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	p.mustUp()
	for i, want := range []string{"v1", "v2"} {
		if out, code := p.bouncer("undo"); code != 0 {
			t.Fatalf("undo %d: %d\n%s", i+1, code, out)
		}
		if body := mustGet(t, url(port, "/")); !strings.Contains(body, want) {
			t.Fatalf("after undo %d GET / = %q, want %s", i+1, body, want)
		}
		top := strings.Fields(p.history("api")[0])
		if rev := fmt.Sprint(3 + i); top[0] != rev || top[1] != "*" || top[3] != "bouncer-e2e-app:"+want {
			t.Fatalf("after undo %d history top %q, want revision %s of %s", i+1, top, rev, want)
		}
	}
}

func TestUndoUsesCurrentEnvFile(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    env_file: [app.env]
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s, drain_method_params: { delay: 2s } }
`, port, fast))
	env := filepath.Join(p.dir, "app.env")
	os.WriteFile(env, []byte("GREETING=one\n"), 0o644)
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	p.mustUp()
	os.WriteFile(env, []byte("GREETING=two\n"), 0o644)
	if out, code := p.bouncer("undo"); code != 0 {
		t.Fatalf("undo: %d\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("GET / = %q", body)
	}
	if got := strings.TrimSpace(mustGet(t, url(port, "/env?k=GREETING"))); got != "two" {
		t.Fatalf("undone replica has GREETING=%q, want the env file's current %q", got, "two")
	}
}

func TestEngineRestartRecovers(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("needs systemd")
	}
	port := freePort(t)
	p := project(t, strings.Replace(api(port, 2, fast), "    deploy:", "    restart: unless-stopped\n    deploy:", 1))
	p.mustUp()
	if out, err := exec.Command("systemctl", "restart", "docker").CombinedOutput(); err != nil {
		t.Fatalf("restart docker: %v\n%s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _, err := get(url(port, "/"))
		if err == nil && code == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no traffic 5s after the engine restart: %d %v", code, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSharedPortsAndAdminCollision(t *testing.T) {
	p := project(t, fmt.Sprintf(`
services:
  a:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    x-bouncer: { %[1]s }
  b:
    image: bouncer-e2e-app:v2
    expose: ["8080"]
    x-bouncer: { %[1]s }
  c:
    image: bouncer-e2e-app:v1
    environment: { PORT: "9901" }
    expose: ["9901"]
    x-bouncer: { %[1]s }
  client:
    image: curlimages/curl:8.16.0
    entrypoint: ["sleep", "infinity"]
    stop_signal: SIGKILL
`, fast))
	p.mustUp()
	ids := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=client")
	if len(ids) != 1 {
		t.Fatalf("client containers %v", ids)
	}
	for svc, target := range map[string]string{"a": "a:8080", "b": "b:8080", "c": "c:9901"} {
		out := strings.Fields(docker(t, "exec", ids[0], "curl", "-fsS", "--max-time", "5", "http://"+target+"/"))
		// The app answers with its hostname, the container ID prefix.
		if len(out) != 2 || !slices.ContainsFunc(p.replicas(svc), func(id string) bool { return strings.HasPrefix(id, out[0]) }) {
			t.Fatalf("%s answered %q, not a replica of %s", target, out, svc)
		}
	}
}

func TestNoDockerHealthcheck(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast+`, drain_method_params: { delay: 5s }`))
	p.mustUp()
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("bounce: %d\n%s", code, out)
	}
	close(stop)
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
}

func TestDownThenUp(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, bounce_method: downthenup, drain_method_params: { delay: 2s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	b := p.start("up")
	most := 0
	for running := true; running; {
		select {
		case <-b.done:
			running = false
		case <-time.After(50 * time.Millisecond):
		}
		if n := len(p.replicas("api")); n > most {
			most = n
		}
	}
	if out, code := b.output(), b.code; code != 0 {
		t.Fatalf("bounce: %d\n%s", code, out)
	}
	if most != 1 {
		t.Fatalf("up to %d replicas existed at once, want 1", most)
	}
	if img := p.images("api"); img["bouncer-e2e-app:v2"] != 1 || len(img) != 1 {
		t.Fatalf("replica images %v", img)
	}
}

func TestLargeLabels(t *testing.T) {
	port := freePort(t)
	big := strings.Repeat("x", 10*1024)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    environment: { BIG: %s, BOUNCE: "0" }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s, history_max: 3, drain_method_params: { delay: 1s } }
`, big, port, fast))
	p.mustUp()
	for i := 1; i <= 4; i++ {
		p.write(strings.Replace(p.yaml, fmt.Sprintf(`BOUNCE: "%d"`, i-1), fmt.Sprintf(`BOUNCE: "%d"`, i), 1))
		p.mustUp()
	}
	if h := p.history("api"); len(h) != 4 || !strings.HasPrefix(h[0], "5 *") {
		t.Fatalf("history (current + 3): %q", h)
	}
	docker(t, "inspect", p.replicas("api")[0])
	out, code := p.bouncer("ls")
	if code != 0 || !strings.Contains(out, p.name) {
		t.Fatalf("ls: %d\n%s", code, out)
	}
}
