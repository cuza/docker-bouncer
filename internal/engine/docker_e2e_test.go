//go:build e2e

package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/envoy"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/moby/moby/client"
)

const e2eProject = "bouncer-engine-e2e"

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// One real Envoy proxy and one replica: proves the Docker adapter and that
// cluster_manager.cds.update_success moves on file rewrites, not DNS refreshes.
func TestDockerAndProxy(t *testing.T) {
	ctx := context.Background()
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	e := NewDocker(c)
	s := svc()
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", e2eProject+"-proxy", e2eProject+"-app-1").Run()
		exec.Command("docker", "network", "rm", e2eProject).Run()
	})
	run(t, "network", "create", e2eProject)
	project := "--label=com.docker.compose.project=" + e2eProject
	run(t, "run", "-d", "--name", e2eProject+"-app-1", "--network", e2eProject, project,
		"--label="+transform.LabelRole+"="+transform.RoleReplica, "--label="+transform.LabelService+"=api",
		"alpine:3", "sleep", "300")
	ep := envoy.Entrypoint()
	proxyID := run(t, "run", "-d", "--name", e2eProject+"-proxy", "--network", e2eProject, project,
		"--label="+transform.LabelRole+"="+transform.RoleProxy, "--label="+transform.LabelService+"=api",
		"-e", "BOUNCER_BOOTSTRAP="+envoy.Bootstrap(s), "-e", "BOUNCER_CDS="+envoy.Clusters(s, []string{e2eProject + "-app-1"}),
		"--entrypoint", ep[0], "envoyproxy/envoy:v1.39.1", ep[1], ep[2])

	reps, err := e.Replicas(ctx, e2eProject, "api")
	if err != nil || len(reps) != 1 || reps[0].Name != e2eProject+"-app-1" || !reps[0].Running || len(reps[0].IPs) != 1 || reps[0].Created.IsZero() {
		t.Fatalf("replicas %+v %v", reps, err)
	}
	px, err := e.Container(ctx, e2eProject, map[string]string{transform.LabelRole: transform.RoleProxy})
	if err != nil || px == nil || px.ID != proxyID {
		t.Fatalf("proxy %+v %v", px, err)
	}
	if out, err := e.Exec(ctx, reps[0].ID, nil, "cat", "/proc/net/tcp"); err != nil || !strings.Contains(out, "local_address") {
		t.Fatalf("exec %q %v", out, err)
	}
	if _, err := e.Exec(ctx, reps[0].ID, nil, "sh", "-c", "echo boom >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "exit 3: boom") {
		t.Fatalf("exec error %v", err)
	}

	p := NewProxy(e, proxyID, s).(*proxy)
	var s0 int64
	for i := 0; ; i++ { // wait for the admin port
		if s0, _, err = p.counters(ctx); err == nil {
			break
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second) // three DNS refreshes of the replica hostname
	if s1, _, _ := p.counters(ctx); s1 != s0 {
		t.Fatalf("update_success moved without a file rewrite: %d -> %d", s0, s1)
	}
	if err := p.SetList(ctx, []string{e2eProject + "-app-1", "other"}); err != nil {
		t.Fatal(err)
	}
	if s2, _, _ := p.counters(ctx); s2 != s0+1 {
		t.Fatalf("update_success %d -> %d, want +1", s0, s2)
	}
	if h, err := p.Health(ctx); err != nil || len(h) == 0 {
		t.Fatalf("health %v %v", h, err)
	} else {
		t.Logf("health %v", h)
	}
	t.Logf("update_success %d -> %d after one SetList; unchanged across 3s of DNS refreshes", s0, s0+1)

	waited := make(chan struct{})
	go func() { e.Wait(ctx, e2eProject, 10*time.Second); close(waited) }()
	time.Sleep(500 * time.Millisecond)
	if err := e.Stop(ctx, reps[0].ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return on a container event")
	}
	if err := e.Remove(ctx, reps[0].ID); err != nil {
		t.Fatal(err)
	}
	if reps, _ := e.Replicas(ctx, e2eProject, "api"); len(reps) != 0 {
		t.Fatalf("replica not removed: %+v", reps)
	}
}
