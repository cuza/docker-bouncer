package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/envoy"
)

type Proxy interface {
	SetList(ctx context.Context, hostnames []string) error
	Current(ctx context.Context) (string, error) // the cluster file as it is now
	Health(ctx context.Context) (map[string]bool, error)
	Conns(ctx context.Context, ips []string) (int, error)
	HTTP(ctx context.Context, method, host string, port int, path string) (int, error)
}

type proxy struct {
	e    Engine
	id   string
	svc  config.Service
	poll time.Duration
}

func NewProxy(e Engine, containerID string, svc config.Service) Proxy {
	return &proxy{e: e, id: containerID, svc: svc, poll: 200 * time.Millisecond}
}

// request speaks HTTP/1.1 through bash's /dev/tcp inside the proxy container
// (the Envoy image has bash but no curl; Envoy's admin answers HTTP/1.0 with 426).
func (p *proxy) request(ctx context.Context, method, host string, port int, path string) (int, string, error) {
	out, err := p.e.Exec(ctx, p.id, []string{"H=" + host, "P=" + strconv.Itoa(port), "M=" + method, "Q=" + path},
		"bash", "-c", `exec 3<>/dev/tcp/$H/$P && printf '%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' "$M" "$Q" "$H" >&3 && cat <&3`)
	if err != nil {
		return 0, "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(strings.NewReader(out)), &http.Request{Method: method})
	if err != nil {
		return 0, "", fmt.Errorf("bad response from %s:%d%s: %w", host, port, path, err)
	}
	body, err := io.ReadAll(res.Body) // undoes chunked encoding
	if err != nil {
		return 0, "", fmt.Errorf("bad response from %s:%d%s: %w", host, port, path, err)
	}
	return res.StatusCode, string(body), nil
}

func (p *proxy) admin(ctx context.Context, path string) (string, error) {
	code, body, err := p.request(ctx, "GET", "127.0.0.1", p.svc.AdminPort, path)
	if err == nil && code != 200 {
		err = fmt.Errorf("envoy admin %s: HTTP %d", path, code)
	}
	return body, err
}

func (p *proxy) counters(ctx context.Context) (success, rejected int64, err error) {
	body, err := p.admin(ctx, "/stats?filter=^cluster_manager\\.cds\\.update_(success|rejected)$")
	if err != nil {
		return 0, 0, err
	}
	success, _ = envoy.ParseCounter(body, "cluster_manager.cds.update_success")
	rejected, _ = envoy.ParseCounter(body, "cluster_manager.cds.update_rejected")
	return success, rejected, nil
}

// SetList rewrites the cluster file atomically and returns once Envoy has
// applied it, so a drain never starts before routing changed.
func (p *proxy) SetList(ctx context.Context, hostnames []string) error {
	s0, r0, err := p.counters(ctx)
	if err != nil {
		return err
	}
	tmp := envoy.ClusterDir + "/.cds.tmp"
	if _, err := p.e.Exec(ctx, p.id, []string{"CDS=" + envoy.Clusters(p.svc, hostnames)},
		"bash", "-c", `printf '%s' "$CDS" > `+tmp+` && mv `+tmp+` `+envoy.ClusterFile); err != nil {
		return fmt.Errorf("write cluster file: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, r, err := p.counters(ctx)
		if err != nil {
			return err
		}
		if r > r0 {
			return fmt.Errorf("envoy rejected the cluster update for %s", p.svc.Name)
		}
		if s > s0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("envoy did not apply the cluster update for %s within 10s", p.svc.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.poll):
		}
	}
}

func (p *proxy) Current(ctx context.Context) (string, error) {
	return p.e.Exec(ctx, p.id, nil, "cat", envoy.ClusterFile)
}

func (p *proxy) Health(ctx context.Context) (map[string]bool, error) {
	body, err := p.admin(ctx, "/clusters")
	if err != nil {
		return nil, err
	}
	return envoy.ParseClusters(body), nil
}

func (p *proxy) Conns(ctx context.Context, ips []string) (int, error) {
	out, err := p.e.Exec(ctx, p.id, nil, "cat", "/proc/net/tcp")
	if err != nil {
		return 0, err
	}
	ports := make([]int, 0, len(p.svc.Ports))
	for _, port := range p.svc.Ports {
		ports = append(ports, port.Target)
	}
	return envoy.ConnCount(out, ips, ports), nil
}

func (p *proxy) HTTP(ctx context.Context, method, host string, port int, path string) (int, error) {
	code, _, err := p.request(ctx, method, host, port, path)
	return code, err
}
