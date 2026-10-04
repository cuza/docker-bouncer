//go:build e2e

package envoy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validate(t *testing.T, mount, config string) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "-v", mount, "--entrypoint", "envoy", "envoyproxy/envoy:v1.39.1",
		"--mode", "validate", "--config-yaml", config).CombinedOutput()
	if err != nil {
		t.Fatalf("envoy rejected the config: %v\n%s", err, out)
	}
}

// Envoy's own validator: proves the generated JSON is a config Envoy accepts.
func TestEnvoyValidatesBootstrapAndClusters(t *testing.T) {
	dir := t.TempDir()
	cds := Clusters(service(), []string{"a", "b"})
	if err := os.WriteFile(filepath.Join(dir, "cds.json"), []byte(cds), 0o644); err != nil {
		t.Fatal(err)
	}
	// Envoy accepts the upgrade types and the disabled stream idle timeout.
	boot := Bootstrap(service())
	if !strings.Contains(boot, `"stream_idle_timeout":"0s"`) || !strings.Contains(boot, `"upgrade_type":"derp"`) {
		t.Fatalf("bootstrap lacks the upgrade settings: %s", boot)
	}
	validate(t, dir+":"+ClusterDir+":ro", boot)

	// Validate mode never reads the CDS file, so check the clusters as static ones.
	for _, cds := range []string{cds, SeedClusters(service())} {
		var doc struct {
			Resources []map[string]any `json:"resources"`
		}
		if err := json.Unmarshal([]byte(cds), &doc); err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Resources {
			delete(c, "@type")
		}
		b, err := json.Marshal(map[string]any{"static_resources": map[string]any{"clusters": doc.Resources}})
		if err != nil {
			t.Fatal(err)
		}
		validate(t, dir+":"+ClusterDir+":ro", string(b))
	}
}

// The proxy's entrypoint opens dual-stack listeners where IPv6 is disabled by
// sysctl (an IPv4-only network), and IPv4 ones on a kernel without IPv6
// (simulated: the check reads a path that does not exist).
func TestEntrypointListeners(t *testing.T) {
	for _, tc := range []struct{ name, check, want string }{
		{"sysctl-disabled", hasIPv6, "[::]:8080"},
		{"no-kernel-ipv6", `[ -e /nonexistent ]`, "0.0.0.0:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep := Entrypoint()
			script := strings.Replace(ep[2], hasIPv6, tc.check, 1)
			name := "bouncer-e2e-envoy-" + tc.name
			exec.Command("docker", "rm", "-f", name).Run()
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
			out, err := exec.Command("docker", "run", "-d", "--name", name,
				"--sysctl", "net.ipv6.conf.all.disable_ipv6=1", "--sysctl", "net.ipv6.conf.lo.disable_ipv6=1",
				"-e", "BOUNCER_BOOTSTRAP="+Bootstrap(service()), "-e", "BOUNCER_CDS="+SeedClusters(service()),
				"--entrypoint", ep[0], "envoyproxy/envoy:v1.39.1", ep[1], script).CombinedOutput()
			if err != nil {
				t.Fatalf("docker run: %v\n%s", err, out)
			}
			var got string
			for range 50 {
				b, _ := exec.Command("docker", "exec", name, "bash", "-c",
					`exec 3<>/dev/tcp/127.0.0.1/9901 && printf 'GET /listeners HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n' >&3 && cat <&3 && `+
						`exec 4<>/dev/tcp/127.0.0.1/8080 && printf 'GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n' >&4 && head -1 <&4`).CombinedOutput()
				if got = string(b); strings.Contains(got, "HTTP/1.1 503") {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !strings.Contains(got, "port-8080::"+tc.want) || !strings.Contains(got, "HTTP/1.1 503") {
				logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
				t.Fatalf("want a listener on %s answering over IPv4, got:\n%s\nlogs:\n%s", tc.want, got, logs)
			}
		})
	}
}
