package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/envoy"
)

type fakeEngine struct {
	execs   [][]string
	envs    [][]string
	replies []string
}

func (f *fakeEngine) Replicas(context.Context, string, string) ([]Replica, error) { return nil, nil }
func (f *fakeEngine) Container(context.Context, string, map[string]string) (*Replica, error) {
	return nil, nil
}
func (f *fakeEngine) Stop(context.Context, string) error          { return nil }
func (f *fakeEngine) Remove(context.Context, string) error        { return nil }
func (f *fakeEngine) Wait(context.Context, string, time.Duration) {}
func (f *fakeEngine) Exec(_ context.Context, _ string, env []string, cmd ...string) (string, error) {
	f.execs = append(f.execs, cmd)
	f.envs = append(f.envs, env)
	if len(f.replies) == 0 {
		return "", fmt.Errorf("unexpected exec %v", cmd)
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func svc() config.Service {
	return config.Service{Name: "api", AdminPort: 9901, Spec: config.Spec{HealthPath: "/health"},
		Ports: []config.Port{{Target: 8080}}}
}

func http200(body string) string { return "HTTP/1.0 200 OK\r\nX: y\r\n\r\n" + body }

func TestSetListWaitsForUpdate(t *testing.T) {
	f := &fakeEngine{replies: []string{
		http200("cluster_manager.cds.update_success: 4\ncluster_manager.cds.update_rejected: 0\n"), // before
		"", // the write
		http200("cluster_manager.cds.update_success: 4\ncluster_manager.cds.update_rejected: 0\n"), // not yet
		http200("cluster_manager.cds.update_success: 5\ncluster_manager.cds.update_rejected: 0\n"), // applied
	}}
	p := NewProxy(f, "proxy-id", svc()).(*proxy)
	p.poll = time.Millisecond
	if err := p.SetList(context.Background(), []string{"proj-api-app-2"}); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 4 {
		t.Fatalf("want 4 execs, got %d", len(f.execs))
	}
	if !strings.Contains(strings.Join(f.envs[1], " "), "proj-api-app-2") {
		t.Fatal("cluster JSON must be passed in the exec environment")
	}
	write := strings.Join(f.execs[1], " ")
	tmp := envoy.ClusterDir + "/.cds.tmp"
	if !strings.Contains(write, "> "+tmp+" && mv "+tmp+" "+envoy.ClusterFile) {
		t.Fatalf("write must go to a temp file in ClusterDir, then mv to ClusterFile: %s", write)
	}
}

func TestSetListFailsOnRejection(t *testing.T) {
	f := &fakeEngine{replies: []string{
		http200("cluster_manager.cds.update_success: 4\ncluster_manager.cds.update_rejected: 0\n"),
		"",
		http200("cluster_manager.cds.update_success: 4\ncluster_manager.cds.update_rejected: 1\n"),
	}}
	p := NewProxy(f, "proxy-id", svc()).(*proxy)
	p.poll = time.Millisecond
	if err := p.SetList(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("got %v", err)
	}
}

func TestHealthParsesClusters(t *testing.T) {
	f := &fakeEngine{replies: []string{http200(
		"port-8080::10.0.0.2:8080::hostname::proj-api-app-1\nport-8080::10.0.0.2:8080::health_flags::healthy\n")}}
	h, err := NewProxy(f, "p", svc()).Health(context.Background())
	if err != nil || !h["proj-api-app-1"] {
		t.Fatalf("%v %v", h, err)
	}
}

func TestConnsReadsProcNetTCP(t *testing.T) {
	f := &fakeEngine{replies: []string{
		"  sl local rem st\n   0: 0200000A:A1B2 0200000A:1F90 01 0\n"}}
	n, err := NewProxy(f, "p", svc()).Conns(context.Background(), []string{"10.0.0.2"})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if f.execs[0][0] != "cat" || f.execs[0][1] != "/proc/net/tcp" {
		t.Fatalf("exec %v", f.execs[0])
	}
}

// Envoy's admin answers HTTP/1.1 chunked; a line split across chunks must survive.
func TestHealthDecodesChunked(t *testing.T) {
	f := &fakeEngine{replies: []string{"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n" +
		"1e\r\nport-8080::10.0.0.2:8080::host\r\n" +
		"58\r\nname::proj-api-app-1\nport-8080::10.0.0.2:8080::health_flags::healthy\nport-8080::x::y::z\n\r\n0\r\n\r\n"}}
	h, err := NewProxy(f, "p", svc()).Health(context.Background())
	if err != nil || !h["proj-api-app-1"] {
		t.Fatalf("%v %v", h, err)
	}
}
