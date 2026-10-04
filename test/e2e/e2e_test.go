//go:build e2e

package e2e

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	p := project(t, api(port, 2, `min_task_uptime: 1s, bounce_health_timeout: 15s, healthcheck: { uri: /health }, drain_method_params: { delay: 2s }`))
	p.mustUp()
	p.write(strings.Replace(p.yaml, "image: bouncer-e2e-app:v1", "image: bouncer-e2e-app:v2\n    environment: { UNHEALTHY: \"1\" }", 1))
	out, code := p.bouncer("up")
	if code != 1 {
		t.Fatalf("bounce of a never-healthy version: exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "never passed GET /health (expects 2xx) within 15s (last answer: HTTP 503)") {
		t.Fatalf("the failure must name the check:\n%s", out)
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
	out, code := p.bouncer("up", "--progress", "plain")
	if code != 0 {
		t.Fatalf("second up: %d\n%s", code, out)
	}
	if strings.Contains(out, "Proxy definition changed") || strings.Contains(out, "Proxy config updated") {
		t.Fatalf("second up touched the proxy:\n%s", out)
	}
	if after := p.ids(); !mapsEqual(before, after) {
		t.Fatalf("container IDs changed:\nbefore %v\nafter  %v", before, after)
	}
	if h := p.history("api"); len(h) != 1 {
		t.Fatalf("history: %q", h)
	}
}

// A health config change is applied to the running proxy in place: nothing
// is recreated and no request fails.
func TestHealthChangeDoesNotRecreateProxy(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 2, fast))
	p.mustUp()
	before := p.ids()
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	p.write(strings.Replace(p.yaml, fast, fast+", healthcheck: { uri: /health }", 1))
	out, code := p.bouncer("up", "--progress", "plain")
	close(stop)
	if code != 0 {
		t.Fatalf("up: %d\n%s", code, out)
	}
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	if strings.Contains(out, "Proxy definition changed") || !strings.Contains(out, "Proxy config updated") {
		t.Fatalf("want an in-place update:\n%s", out)
	}
	if after := p.ids(); !mapsEqual(before, after) {
		t.Fatalf("container IDs changed:\nbefore %v\nafter  %v", before, after)
	}
	if cds := docker(t, "exec", p.proxy("api"), "cat", "/etc/bouncer/dyn/cds.json"); !strings.Contains(cds, `"path":"/health"`) {
		t.Fatalf("cluster file lacks the new path: %s", cds)
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

// echo is what the app's /headers saw, through the proxy.
type echo struct {
	Header http.Header `json:"header"`
	Host   string      `json:"host"`
}

func headersVia(t *testing.T, url string, h map[string]string) echo {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var e echo
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// xff splits X-Forwarded-For into its addresses.
func xff(e echo) []string {
	var out []string
	for _, a := range strings.Split(e.Header.Get("X-Forwarded-For"), ",") {
		out = append(out, strings.TrimSpace(a))
	}
	return out
}

func TestForwardedHeaders(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast))
	p.mustUp()
	u := url(port, "/headers")

	direct := headersVia(t, u, nil)
	if a := xff(direct); len(a) != 1 || net.ParseIP(a[0]) == nil {
		t.Errorf("direct: X-Forwarded-For %q, want the caller's address", direct.Header.Get("X-Forwarded-For"))
	}
	if got := direct.Header.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("direct: X-Forwarded-Proto %q, want http", got)
	}
	if want := fmt.Sprintf("127.0.0.1:%d", port); direct.Host != want {
		t.Errorf("direct: Host %q, want %q", direct.Host, want)
	}

	// A TLS-terminating proxy in front.
	behind := headersVia(t, u, map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.7", "X-Request-Id": "req-1"})
	if a := xff(behind); len(a) != 2 || a[0] != "203.0.113.7" || net.ParseIP(a[1]) == nil {
		t.Errorf("behind a proxy: X-Forwarded-For %q, want 203.0.113.7 plus the caller's address", behind.Header.Get("X-Forwarded-For"))
	}
	if got := behind.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("behind a proxy: X-Forwarded-Proto %q, want https kept", got)
	}
	if got := behind.Header.Get("X-Request-Id"); got != "req-1" {
		t.Errorf("behind a proxy: X-Request-Id %q, want req-1 kept", got)
	}
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
		if rev := docker(t, "inspect", "-f", `{{index .Config.Labels "dev.cuza.bouncer.revision"}}`, id); rev != "1" {
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
	b := p.start("up", "--progress", "plain")
	b.waitFor(t, "Draining", 2*time.Minute)
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
	b := p.start("up", "--progress", "plain")
	b.waitFor(t, "Draining", 2*time.Minute)
	// What Ctrl-C on a TTY delivers to the plugin (the CLI leaves it to the kernel).
	if err := syscall.Kill(b.plugin(t), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if out, code := b.wait(t, 15*time.Second); code == 0 {
		t.Fatalf("interrupted up exited 0:\n%s", out)
	}
	if ids := p.containers("dev.cuza.bouncer.role=lock", "com.docker.compose.project="+p.name); len(ids) != 0 {
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

// The cron case: the file is unchanged but its tag now names another image.
// up bounces to it, the replicas keep the tag, and undo runs the old image
// again by its ID.
func TestMovedTagBounces(t *testing.T) {
	port := freePort(t)
	tag := fmt.Sprintf("bouncer-e2e-app:moving-%d", port)
	docker(t, "tag", "bouncer-e2e-app:v1", tag)
	t.Cleanup(func() { exec.Command("docker", "rmi", tag).Run() })
	p := project(t, strings.Replace(api(port, 1, fast+`, drain_method_params: { delay: 1s }`), "bouncer-e2e-app:v1", tag, 1))
	p.mustUp()
	p.mustUp()
	if h := p.history("api"); len(h) != 1 {
		t.Fatalf("an unchanged image is no new revision: %q", h)
	}
	docker(t, "tag", "bouncer-e2e-app:v2", tag)
	p.mustUp()
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v2") {
		t.Fatalf("after the tag moved GET / = %q, want v2", body)
	}
	if img := p.images("api"); img[tag] != 1 || len(img) != 1 {
		t.Fatalf("replicas run %v, want the tag as written", img)
	}
	h := p.history("api")
	if v2 := docker(t, "image", "inspect", "-f", "{{.Id}}", "bouncer-e2e-app:v2")[len("sha256:"):][:12]; len(h) != 2 || !strings.Contains(h[0], tag+" ("+v2+")") {
		t.Fatalf("a moved tag is a new revision showing its image: %q", h)
	}
	if out, code := p.bouncer("undo"); code != 0 {
		t.Fatalf("undo: %d\n%s", code, out)
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("after undo GET / = %q, want v1", body)
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

// The default check is GET / accepting any non-5xx, so an image with no
// health endpoint and no Docker healthcheck deploys and bounces as is.
func TestDefaultHealthWorksWithoutHealthEndpoint(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  web:
    image: nginx:1.29-alpine
    ports: ["127.0.0.1:%d:80"]
    x-bouncer: { %s, drain_method_params: { delay: 2s } }
`, port, fast))
	p.mustUp()
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	p.write(strings.Replace(p.yaml, "nginx:1.29-alpine", "nginx:1.30-alpine", 1))
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("bounce: %d\n%s", code, out)
	}
	close(stop)
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	if img := p.images("web"); img["nginx:1.30-alpine"] != 1 || len(img) != 1 {
		t.Fatalf("replica images %v", img)
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

// TestLargeLabels: each revision carries ~27 KiB of incompressible spec, so
// the default history_max (100) gives way to the 64 KiB history budget.
func TestLargeLabels(t *testing.T) {
	port := freePort(t)
	big := func() string {
		b := make([]byte, 20*1024)
		rand.Read(b)
		return hex.EncodeToString(b)
	}
	yaml := func(v string) string {
		return fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    environment: { BIG: %s }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s, drain_method_params: { delay: 1s } }
`, v, port, fast)
	}
	p := project(t, yaml(big()))
	p.mustUp()
	var out string
	for range 3 {
		p.write(yaml(big()))
		var code int
		if out, code = p.bouncer("up"); code != 0 {
			t.Fatalf("up: exit %d\n%s", code, out)
		}
	}
	if !strings.Contains(out, "History trimmed") {
		t.Fatalf("no trim warning:\n%s", out)
	}
	if h := p.history("api"); len(h) != 3 || !strings.HasPrefix(h[0], "4 *") {
		t.Fatalf("history (current + 2 that fit): %q", h)
	}
	docker(t, "inspect", p.replicas("api")[0])
	out, code := p.bouncer("ls")
	if code != 0 || !strings.Contains(out, p.name) {
		t.Fatalf("ls: %d\n%s", code, out)
	}
}

func TestUpNamedServiceOnly(t *testing.T) {
	p := project(t, fmt.Sprintf(`
services:
  a:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    x-bouncer: { %[1]s, drain_method_params: { delay: 1s } }
  b:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    x-bouncer: { %[1]s, drain_method_params: { delay: 1s } }
  side:
    image: alpine:3.22
    command: ["sleep", "infinity"]
    environment: { V: "1" }
    stop_signal: SIGKILL
`, fast))
	p.mustUp()
	before := p.ids()
	// Every service changes; only a may be converged.
	p.write(strings.Replace(strings.ReplaceAll(p.yaml, ":v1", ":v2"), `V: "1"`, `V: "2"`, 1))
	if out, code := p.bouncer("up", "a"); code != 0 {
		t.Fatalf("up a: %d\n%s", code, out)
	}
	if img := p.images("a"); img["bouncer-e2e-app:v2"] != 1 || len(img) != 1 {
		t.Fatalf("a replicas %v, want v2", img)
	}
	after := p.ids()
	for name, id := range before {
		if !strings.HasPrefix(name, "/"+p.name+"-a-") && after[name] != id {
			t.Fatalf("up a touched %s", name)
		}
	}
	if img := p.images("b"); img["bouncer-e2e-app:v1"] != 1 || len(img) != 1 {
		t.Fatalf("b replicas %v, want untouched v1", img)
	}
	if out, code := p.bouncer("up", "nope"); code != 2 {
		t.Fatalf("up nope: exit %d, want 2\n%s", code, out)
	}
}

// json progress is one object per line; -d and --wait are accepted no-ops.
func TestProgressJSON(t *testing.T) {
	p := project(t, api(freePort(t), 1, fast+`, drain_method_params: { delay: 1s }`))
	out, code := p.bouncer("up", "-d", "--wait", "--progress", "json")
	if code != 0 {
		t.Fatalf("up: %d\n%s", code, out)
	}
	converged := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e struct{ Time, Project, Service, Status, Message string }
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not JSON: %q: %v\n%s", line, err, out)
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil || e.Project != p.name {
			t.Fatalf("time/project: %q", line)
		}
		converged = converged || e.Service == "api" && e.Status == "Done" && strings.HasPrefix(e.Message, "Converged")
	}
	if !converged {
		t.Fatalf("no Converged event for api:\n%s", out)
	}
}

// A Service depending on another starts, bounces each side, and stops before
// the other's proxy and replicas.
func TestDependsOnBetweenBouncerServices(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    x-bouncer: { %[2]s }
  web:
    image: bouncer-e2e-app:v1
    depends_on: [api]
    ports: ["127.0.0.1:%[1]d:8080"]
    x-bouncer: { %[2]s, drain_method_params: { delay: 2s } }
`, port, fast))
	p.mustUp()
	before := p.replicas("api")
	stop := make(chan struct{})
	res := load(t, url(port, "/"), stop)
	// Bounce web only; the scoped up also selects api-app but must not bounce it.
	p.write(strings.Replace(p.yaml, "image: bouncer-e2e-app:v1\n    depends_on", "image: bouncer-e2e-app:v2\n    depends_on", 1))
	if out, code := p.bouncer("up", "web"); code != 0 {
		t.Fatalf("bounce web: %d\n%s", code, out)
	}
	close(stop)
	if r := <-res; r.failures != 0 || r.total == 0 {
		t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
	}
	if img := p.images("web"); img["bouncer-e2e-app:v2"] != 1 || len(img) != 1 {
		t.Fatalf("web replicas %v", img)
	}
	if got := p.replicas("api"); !slices.Equal(got, before) {
		t.Fatalf("up web bounced api: %v -> %v", before, got)
	}
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	if out, code := p.bouncer("up"); code != 0 {
		t.Fatalf("bounce api: %d\n%s", code, out)
	}
	if img := p.images("api"); img["bouncer-e2e-app:v2"] != 1 || len(img) != 1 {
		t.Fatalf("api replicas %v", img)
	}
	if out, code := p.bouncer("stop"); code != 0 {
		t.Fatalf("stop: %d\n%s", code, out)
	}
	finished := func(ids ...string) (first, last time.Time) {
		for _, id := range ids {
			f, err := time.Parse(time.RFC3339Nano, docker(t, "inspect", "-f", "{{.State.FinishedAt}}", id))
			if err != nil {
				t.Fatal(err)
			}
			if first.IsZero() || f.Before(first) {
				first = f
			}
			if f.After(last) {
				last = f
			}
		}
		return first, last
	}
	_, webLast := finished(p.replicas("web")...)
	apiFirst, _ := finished(append(p.replicas("api"), p.proxy("api"))...)
	t.Logf("web replicas stopped by %s, api proxy and replicas from %s", webLast.Format(time.StampMicro), apiFirst.Format(time.StampMicro))
	if !webLast.Before(apiFirst) {
		t.Fatal("web replicas must stop before api's proxy and replicas")
	}
}

// A build-only Service is built on the first up, and a changed build arg or
// Dockerfile is a new image and so a bounce, without a failed request.
func TestBuildService(t *testing.T) {
	port := freePort(t)
	label := pruneBuilt(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    build: { context: ./ctx, args: { V: one } }
    deploy: { replicas: 2 }
    ports: ["127.0.0.1:%d:8080"]
    x-bouncer: { %s, drain_method_params: { delay: 2s } }
`, port, fast))
	dockerfile := filepath.Join(p.dir, "ctx", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dockerfile, "FROM bouncer-e2e-app:v1\nLABEL "+label+"\nARG V\nENV VERSION=$V\n")
	p.mustUp()
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "one") {
		t.Fatalf("GET / = %q", body)
	}
	bounce := func(want string) {
		t.Helper()
		stop := make(chan struct{})
		res := load(t, url(port, "/"), stop)
		if out, code := p.bouncer("up"); code != 0 {
			t.Fatalf("bounce to %s: %d\n%s", want, code, out)
		}
		close(stop)
		if r := <-res; r.failures != 0 || r.total == 0 {
			t.Fatalf("%d failed requests out of %d: %v", r.failures, r.total, r.samples)
		}
		if body := mustGet(t, url(port, "/")); !strings.Contains(body, want) {
			t.Fatalf("GET / = %q, want %s", body, want)
		}
	}
	p.write(strings.Replace(p.yaml, "V: one", "V: two", 1))
	bounce("two")
	writeFile(t, dockerfile, "FROM bouncer-e2e-app:v1\nLABEL "+label+"\nENV VERSION=three\n")
	bounce("three")
	if h := p.history("api"); len(h) != 3 {
		t.Fatalf("history: %q", h)
	}
	before := p.replicas("api")
	if out, code := p.bouncer("up"); code != 0 || !slices.Equal(p.replicas("api"), before) {
		t.Fatalf("an unchanged build must not bounce: %d\n%s", code, out)
	}
	writeFile(t, dockerfile, "FROM bouncer-e2e-app:v1\nRUN exit 1\n")
	if out, code := p.bouncer("up"); code != 2 || !slices.Equal(p.replicas("api"), before) {
		t.Fatalf("a failed build: exit %d, want 2 and no change\n%s", code, out)
	}
}

// pruneBuilt returns a LABEL for test Dockerfiles; the images built with it
// are removed once the test's projects are down (call it before project:
// cleanups run last-registered first).
func pruneBuilt(t *testing.T) string {
	l := fmt.Sprintf("bouncer-e2e-build=%d-%d", os.Getpid(), projectSeq.Add(1))
	t.Cleanup(func() { exec.Command("docker", "image", "prune", "-af", "--filter", "label="+l).Run() })
	return l
}

// A build referring to a Service (additional_contexts: service:S) gets S's
// built app image, not its proxy.
func TestBuildContextFromService(t *testing.T) {
	port := freePort(t)
	label := pruneBuilt(t)
	p := project(t, fmt.Sprintf(`
services:
  base:
    build: ./base
    expose: ["8080"]
    x-bouncer: { %[2]s }
  api:
    build: { context: ./api, additional_contexts: { base: "service:base" } }
    ports: ["127.0.0.1:%[1]d:8080"]
    x-bouncer: { %[2]s }
`, port, fast))
	if buildx == "" {
		t.Skip("additional_contexts need BuildKit, which Compose uses only through the buildx plugin")
	}
	cfg := filepath.Join(t.TempDir(), "cli-plugins")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"docker-buildx": buildx, "docker-bouncer": filepath.Join(os.Getenv("DOCKER_CONFIG"), "cli-plugins", "docker-bouncer")} {
		if err := os.Symlink(target, filepath.Join(cfg, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONFIG", filepath.Dir(cfg))
	for dir, body := range map[string]string{
		"base": "FROM bouncer-e2e-app:v1\nLABEL " + label + "\nENV VERSION=from-base\n",
		"api":  "FROM base\nLABEL " + label + "\n",
	} {
		if err := os.MkdirAll(filepath.Join(p.dir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p.dir, dir, "Dockerfile"), body)
	}
	p.mustUp()
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "from-base") {
		t.Fatalf("GET / = %q, want the app built from base's image", body)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A drained replica's anonymous volumes go with it.
func TestAnonymousVolumesDoNotLeak(t *testing.T) {
	p := project(t, api(freePort(t), 2, fast+`, drain_method_params: { delay: 1s }`))
	image := "bouncer-e2e-vol:" + p.name[len("bouncer-e2e-"):]
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader("FROM bouncer-e2e-app:v1\nVOLUME /data\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", image).Run() })
	p.write(strings.Replace(p.yaml, "bouncer-e2e-app:v1", image, 1))
	p.mustUp()
	seen := map[string]bool{}
	mounted := func() {
		for _, id := range p.replicas("api") {
			for _, v := range strings.Fields(docker(t, "inspect", "-f", `{{range .Mounts}}{{.Name}} {{end}}`, id)) {
				seen[v] = true
			}
		}
	}
	mounted()
	base := p.yaml
	for _, v := range []string{"1", "2"} {
		p.write(strings.Replace(base, "x-bouncer:", "environment: { N: \""+v+"\" }\n    x-bouncer:", 1))
		p.mustUp()
		mounted()
	}
	live := map[string]bool{}
	for _, id := range p.replicas("api") {
		for _, v := range strings.Fields(docker(t, "inspect", "-f", `{{range .Mounts}}{{.Name}} {{end}}`, id)) {
			live[v] = true
		}
	}
	if len(seen) != 6 || len(live) != 2 {
		t.Fatalf("volumes seen %d, live %d; want 6 and 2", len(seen), len(live))
	}
	for v := range seen {
		if !live[v] && exec.Command("docker", "volume", "inspect", v).Run() == nil {
			t.Errorf("volume %s of a drained replica still exists", v)
		}
	}
	if out, code := p.bouncer("down"); code != 0 {
		t.Fatalf("down: %d\n%s", code, out)
	}
	for v := range live {
		exec.Command("docker", "volume", "rm", v).Run()
	}
}

// Sharing a namespace or volumes with a Service is rejected before anything runs.
func TestServiceNamespaceReferencesRejected(t *testing.T) {
	for name, tc := range map[string]struct{ api, other, want string }{
		"host":         {"network_mode: host", "", "network_mode host cannot be set on a bouncer service"},
		"none":         {"network_mode: none", "", "network_mode none cannot be set on a bouncer service"},
		"network_mode": {"", "network_mode: service:api", "network_mode service:api points at api's proxy"},
		"ipc":          {"", "ipc: service:api", "ipc service:api points at api's proxy"},
		"pid":          {"", "pid: service:api", "pid service:api points at api's proxy"},
		"volumes_from": {"", "volumes_from: [api]", "volumes_from api points at api's proxy"},
		"container":    {"network_mode: container:other", "", "network_mode container:other cannot be set on a bouncer service"},
		"service":      {"network_mode: service:other", "", "network_mode service:other cannot be set on a bouncer service"},
		"link_local":   {"networks: { default: { link_local_ips: [169.254.0.10] } }", "", "networks.default.link_local_ips cannot be set"},
		"label":        {"", "labels: { dev.cuza.bouncer.role: replica }", "label dev.cuza.bouncer.role uses the reserved"},
	} {
		t.Run(name, func(t *testing.T) {
			p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    %s
    x-bouncer: {}
  other:
    image: alpine:3.22
    command: ["sleep", "infinity"]
    %s
`, tc.api, tc.other))
			out, code := p.bouncer("up")
			if code != 2 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want 2 and %q\n%s", code, tc.want, out)
			}
			if ids := p.containers("com.docker.compose.project=" + p.name); len(ids) != 0 {
				t.Fatalf("containers created: %v", ids)
			}
		})
	}
}

func TestLongNamesRejected(t *testing.T) {
	svc := "a-service-name-long-enough-to-overflow-dns"
	p := project(t, fmt.Sprintf(`
services:
  %s:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    x-bouncer: {}
`, svc))
	out, code := p.bouncer("up")
	if code != 2 || !strings.Contains(out, p.name+"-"+svc+"-app-1000") || !strings.Contains(out, "over the 63-character DNS name limit") {
		t.Fatalf("exit %d, want 2 naming the replica name\n%s", code, out)
	}
	if ids := p.containers("com.docker.compose.project=" + p.name); len(ids) != 0 {
		t.Fatalf("containers created: %v", ids)
	}
}

// A Service up with --profile is left alone by an up without it and shown
// live by ps. down without the profile removes its proxy and replicas but, as
// docker compose down, leaves a plain service of that profile running.
func TestProfiledServiceDown(t *testing.T) {
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    profiles: [database]
    deploy: { replicas: 2 }
    expose: ["8080"]
    x-bouncer: { %s }
  db:
    image: alpine:3.22
    profiles: [database]
    command: ["sleep", "infinity"]
    stop_signal: SIGKILL
  side:
    image: alpine:3.22
    command: ["sleep", "infinity"]
    stop_signal: SIGKILL
`, fast))
	if out, code := p.bouncer("--profile", "database", "up"); code != 0 {
		t.Fatalf("up --profile database: %d\n%s", code, out)
	}
	before := p.replicas("api")
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	if out, code := p.bouncer("up"); code != 0 || !slices.Equal(p.replicas("api"), before) {
		t.Fatalf("up without the profile must not bounce api: %d\n%s", code, out)
	}
	p.write(strings.Replace(p.yaml, ":v2", ":v1", 1))
	out, code := p.bouncer("ps")
	if code != 0 || strings.Count(out, " live ") != 2 || strings.Contains(out, "draining") {
		t.Fatalf("ps without the profile: %d\n%s", code, out)
	}
	if out, code := p.bouncer("down"); code != 0 {
		t.Fatalf("down: %d\n%s", code, out)
	}
	if ids := p.containers("com.docker.compose.project="+p.name, "dev.cuza.bouncer.managed=true"); len(ids) != 0 {
		t.Fatalf("down without the profile left api's proxy or replicas: %v", ids)
	}
	db := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=db")
	if len(db) != 1 || docker(t, "inspect", "-f", "{{.State.Running}}", db[0]) != "true" {
		t.Fatalf("down without the profile must leave db running: %v", db)
	}
	if ids := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.service=side"); len(ids) != 0 {
		t.Fatalf("down left side: %v", ids)
	}
	if out, code := p.bouncer("--profile", "database", "down"); code != 0 {
		t.Fatalf("down --profile database: %d\n%s", code, out)
	}
	if ids := p.containers("com.docker.compose.project=" + p.name); len(ids) != 0 {
		t.Fatalf("down --profile database left %v", ids)
	}
}

// upgrade does a raw HTTP/1.1 upgrade through the proxy and, on 101, checks
// that bytes sent come back. It returns the response status.
func upgrade(t *testing.T, port int, typ string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: %s\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", typ)
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return res.StatusCode
	}
	// The RFC 6455 example key and its accept value.
	if typ == "websocket" && res.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("%s: Sec-WebSocket-Accept %q", typ, res.Header.Get("Sec-WebSocket-Accept"))
	}
	for _, msg := range []string{"ping", "pong"} {
		if _, err := io.WriteString(conn, msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(br, got); err != nil || string(got) != msg {
			t.Fatalf("%s: echo %q %v, want %q", typ, got, err, msg)
		}
	}
	return res.StatusCode
}

// WebSockets pass by default, a type listed in upgrade_types passes, and any
// other upgrade gets Envoy's 403.
func TestUpgradesPassThroughProxy(t *testing.T) {
	port := freePort(t)
	p := project(t, api(port, 1, fast+`, upgrade_types: [DERP]`))
	p.mustUp()
	for typ, want := range map[string]int{"websocket": 101, "derp": 101, "h2c-nope": 403} {
		if got := upgrade(t, port, typ); got != want {
			t.Errorf("Upgrade: %s got %d, want %d", typ, got, want)
		}
	}
}

// On a network with IPv6, clients reach the proxy over either family.
func TestIPv6Clients(t *testing.T) {
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    expose: ["8080"]
    networks: [dual]
    x-bouncer: { %s }
networks:
  dual:
    enable_ipv6: true
`, fast))
	p.mustUp()
	for _, field := range []string{"IPAddress", "GlobalIPv6Address"} {
		ip := docker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{."+field+"}}{{end}}", p.proxy("api"))
		if ip == "" {
			t.Fatalf("proxy has no %s", field)
		}
		u := "http://" + net.JoinHostPort(ip, "8080") + "/"
		if out := docker(t, "run", "--rm", "--network", p.name+"_dual", "curlimages/curl:8.16.0", "-sSg", "--max-time", "10", u); !strings.Contains(out, "v1") {
			t.Fatalf("GET %s = %q", u, out)
		}
	}
}

// pre_start hooks run before the new replicas start on every bounce and
// undo, as Compose runs them for a recreated service; a failing hook fails
// the bounce and the old replicas keep serving.
func TestPreStartRunsOnBounceAndUndo(t *testing.T) {
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: bouncer-e2e-app:v1
    ports: ["127.0.0.1:%d:8080"]
    volumes: ["./hooks:/hooks", "/scratch"]
    pre_start:
      - entrypoint: ["sh", "-c", "echo $$VERSION >> /hooks/log"]
    x-bouncer: { %s, drain_method_params: { delay: 1s } }
`, port, fast))
	if err := os.MkdirAll(filepath.Join(p.dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := func(want ...string) {
		t.Helper()
		b, _ := os.ReadFile(filepath.Join(p.dir, "hooks", "log"))
		if got := strings.Fields(string(b)); !slices.Equal(got, want) {
			t.Fatalf("hooks ran for %q, want %q", got, want)
		}
	}
	p.mustUp()
	ran("v1")
	p.mustUp()
	ran("v1") // no bounce, no hook
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	p.mustUp()
	ran("v1", "v2")
	if out, code := p.bouncer("undo"); code != 0 {
		t.Fatalf("undo: %d\n%s", code, out)
	}
	ran("v1", "v2", "v1")
	p.write(strings.Replace(strings.Replace(p.yaml, ":v2", ":v1", 1), ">> /hooks/log", ">> /hooks/log; echo migration-failed >&2; exit 3", 1))
	before := p.replicas("api")
	volumes := docker(t, "volume", "ls", "-q")
	if out, code := p.bouncer("up"); code != 1 || !strings.Contains(out, "pre_start[0]: exited with 3") || !strings.Contains(out, "migration-failed") {
		t.Fatalf("a failing hook: exit %d, want 1 with its exit code and output\n%s", code, out)
	}
	// The hook and the unstarted replica are gone, with the replica's
	// anonymous volume.
	if ids := p.containers("com.docker.compose.project="+p.name, "com.docker.compose.hook=pre_start"); len(ids) != 0 {
		t.Fatalf("hook containers left: %v", ids)
	}
	if after := docker(t, "volume", "ls", "-q"); after != volumes {
		t.Fatalf("volumes leaked:\nbefore %s\nafter %s", volumes, after)
	}
	ran("v1", "v2", "v1", "v1")
	if !slices.Equal(p.replicas("api"), before) {
		t.Fatalf("a failing hook must leave the old replicas alone")
	}
	if body := mustGet(t, url(port, "/")); !strings.Contains(body, "v1") {
		t.Fatalf("GET / = %q", body)
	}
}

// Replicas get a provider's injected environment, as a plain dependent does,
// on the first up and in every step of a bounce.
func TestProviderEnvironmentReachesReplicas(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	writeFile(t, provider, "#!/bin/sh\ncase \" $* \" in *\" up \"*) echo '{\"type\":\"setenv\",\"message\":\"URL=from-provider\"}';; esac\n")
	if err := os.Chmod(provider, 0o755); err != nil {
		t.Fatal(err)
	}
	p := project(t, fmt.Sprintf(`
services:
  db:
    provider: { type: %s }
  api:
    image: bouncer-e2e-app:v1
    depends_on: [db]
    expose: ["8080"]
    deploy: { replicas: 3 }
    x-bouncer: { %s, bounce_overprovision_factor: 0.33, drain_method_params: { delay: 1s } }
`, provider, fast))
	check := func(when string) {
		t.Helper()
		for _, id := range p.replicas("api") {
			if got := docker(t, "exec", id, "sh", "-c", "echo $DB_URL"); got != "from-provider" {
				t.Fatalf("DB_URL = %q in %s after %s", got, id, when)
			}
		}
	}
	p.mustUp()
	check("the first up")
	p.write(strings.Replace(p.yaml, ":v1", ":v2", 1))
	p.mustUp()
	check("a bounce in three steps")
}

// A Service with platform: is pulled for that platform when the local copy
// of its tag is another platform's.
func TestPlatformPullsWhenOnlyAnotherIsLocal(t *testing.T) {
	const image = "traefik/whoami:v1.11.0"
	native, other := "linux/arm64", "linux/amd64"
	if docker(t, "info", "-f", "{{.Architecture}}") == "x86_64" {
		native, other = other, native
	}
	exec.Command("docker", "rmi", "-f", image).Run()
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", image).Run() })
	docker(t, "pull", "-q", "--platform", native, image)
	port := freePort(t)
	p := project(t, fmt.Sprintf(`
services:
  api:
    image: %s
    platform: %s
    ports: ["127.0.0.1:%d:80"]
    x-bouncer: { %s }
`, image, other, port, fast))
	p.mustUp()
	mustGet(t, url(port, "/"))
	reps := p.replicas("api")
	if len(reps) != 1 {
		t.Fatalf("%d replicas", len(reps))
	}
	// The manifest a container runs is known with the containerd image store.
	got := docker(t, "inspect", "-f", "{{with .ImageManifestDescriptor}}{{.Platform.OS}}/{{.Platform.Architecture}}{{end}}", reps[0])
	if got != "" && got != other {
		t.Fatalf("replica runs %s, want %s", got, other)
	}
}
