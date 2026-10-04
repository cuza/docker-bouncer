//go:build e2e

package envoy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
