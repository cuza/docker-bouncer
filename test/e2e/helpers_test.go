//go:build e2e

// Package e2e runs the built plugin against a real Docker engine:
// go test -tags e2e -timeout 20m ./test/e2e/...
package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var projectSeq atomic.Int64

// TestMain builds the test app images and the plugin into a private
// DOCKER_CONFIG, so `docker bouncer` runs this tree's code and the user's
// ~/.docker is never touched.
func TestMain(m *testing.M) {
	os.Exit(func() int {
		if err := setup(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e setup:", err)
			return 1
		}
		return m.Run()
	}())
}

func setup() error {
	// Images first, with the user's own docker config (buildx lives there).
	for _, v := range []string{"v1", "v2"} {
		if out, err := exec.Command("docker", "build", "-q", "-t", "bouncer-e2e-app:"+v, "--build-arg", "VERSION="+v, "app").CombinedOutput(); err != nil {
			return fmt.Errorf("build app %s: %v\n%s", v, err, out)
		}
	}
	for _, img := range []string{"envoyproxy/envoy:v1.39.1", "curlimages/curl:8.16.0"} {
		if exec.Command("docker", "image", "inspect", img).Run() != nil {
			if out, err := exec.Command("docker", "pull", "-q", img).CombinedOutput(); err != nil {
				return fmt.Errorf("pull %s: %v\n%s", img, err, out)
			}
		}
	}
	// The private config has no contexts: pin the daemon of the current one.
	if os.Getenv("DOCKER_HOST") == "" {
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			return fmt.Errorf("docker context inspect: %v", err)
		}
		os.Setenv("DOCKER_HOST", strings.TrimSpace(string(out)))
	}
	dir, err := os.MkdirTemp("", "bouncer-e2e-config")
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, ".docker", "config.json")); err == nil { // credentials
		if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
			return err
		}
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "cli-plugins", "docker-bouncer"), ".")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build plugin: %v\n%s", err, out)
	}
	os.Setenv("DOCKER_CONFIG", dir)
	os.Unsetenv("DOCKER_CONTEXT")
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Errorf("docker info with the private config: %v\n%s", err, out)
	}
	return nil
}

type proj struct {
	t    *testing.T
	name string
	dir  string
	yaml string
}

// project writes compose.yaml (with {{PORT}} replaced by a free host port,
// see p.port) and removes everything the project created when the test ends.
func project(t *testing.T, yaml string) *proj {
	t.Helper()
	p := &proj{t: t, dir: t.TempDir(),
		name: fmt.Sprintf("bouncer-e2e-%d-%d", os.Getpid()%10000, projectSeq.Add(1))}
	p.write(yaml)
	t.Cleanup(func() {
		if out, code := p.bouncer("down"); code != 0 {
			t.Logf("down: %d\n%s", code, out)
		}
		exec.Command("docker", "rm", "-f", p.name+"-bouncer-lock").Run()
		if ids := p.containers("com.docker.compose.project=" + p.name); len(ids) > 0 {
			t.Logf("leftover containers after down: %v", ids)
			exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
		}
	})
	return p
}

func (p *proj) write(yaml string) {
	p.t.Helper()
	p.yaml = yaml
	if err := os.WriteFile(filepath.Join(p.dir, "compose.yaml"), []byte(yaml), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *proj) cmd(args ...string) *exec.Cmd {
	c := exec.Command("docker", append([]string{"bouncer", "-p", p.name}, args...)...)
	c.Dir = p.dir
	return c
}

// bouncer runs the plugin; output is stdout and stderr combined.
func (p *proj) bouncer(args ...string) (string, int) {
	p.t.Helper()
	out, err := p.cmd(args...).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	case err != nil:
		p.t.Fatalf("docker bouncer %v: %v", args, err)
	}
	return string(out), 0
}

func (p *proj) mustUp() {
	p.t.Helper()
	if out, code := p.bouncer("up"); code != 0 {
		p.t.Fatalf("up: exit %d\n%s", code, out)
	}
}

// containers returns IDs of all containers (running or not) matching the label filters.
func (p *proj) containers(labels ...string) []string {
	args := []string{"ps", "-aq", "--no-trunc"}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}
	out, _ := exec.Command("docker", args...).Output()
	return strings.Fields(string(out))
}

// replicas returns the IDs of the Service's replica containers.
func (p *proj) replicas(svc string) []string {
	return p.containers("com.docker.compose.project="+p.name, "bouncer.role=replica", "bouncer.service="+svc)
}

func (p *proj) proxy(svc string) string {
	p.t.Helper()
	ids := p.containers("com.docker.compose.project="+p.name, "bouncer.role=proxy", "bouncer.service="+svc)
	if len(ids) != 1 {
		p.t.Fatalf("%s: %d proxy containers", svc, len(ids))
	}
	return ids[0]
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// freePort returns a host port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var client = &http.Client{Timeout: 30 * time.Second}

// get returns the status and body of one GET.
func get(url string) (int, string, error) {
	res, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, string(b), err
}

func mustGet(t *testing.T, url string) string {
	t.Helper()
	code, body, err := get(url)
	if err != nil || code != 200 {
		t.Fatalf("GET %s: %d %q %v", url, code, body, err)
	}
	return body
}

type loadResult struct {
	total, failures int
	samples         []string // the first few failures
}

// load sends one GET every 20 ms until stop is closed.
func load(t *testing.T, url string, stop <-chan struct{}) <-chan loadResult {
	t.Helper()
	mustGet(t, url) // the target serves before load starts
	res := make(chan loadResult, 1)
	go func() {
		var r loadResult
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				res <- r
				return
			case <-tick.C:
			}
			r.total++
			code, body, err := get(url)
			if err == nil && code == 200 {
				continue
			}
			r.failures++
			if len(r.samples) < 5 {
				r.samples = append(r.samples, fmt.Sprintf("%s %d %q %v", time.Now().Format("15:04:05.000"), code, body, err))
			}
		}
	}()
	return res
}

// proxyClusters is the proxy's Envoy /clusters, fetched from inside the proxy
// container the way the plugin does it (the admin port is never published).
func (p *proj) proxyClusters(svc string, adminPort int) string {
	p.t.Helper()
	out := docker(p.t, "exec", p.proxy(svc), "bash", "-c",
		fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d && printf 'GET /clusters HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' >&3 && cat <&3`, adminPort))
	if !strings.HasPrefix(out, "HTTP/1.1 200") {
		p.t.Fatalf("/clusters: %s", out)
	}
	return out
}
