//go:build e2e

package transform

import (
	"os/exec"
	"testing"
	"time"
)

// The proxy's Docker healthcheck must pass against a real Envoy.
func TestProxyHealthcheckPassesOnRealEnvoy(t *testing.T) {
	r, err := Apply(project())
	if err != nil {
		t.Fatal(err)
	}
	p := r.Project.Services["api"]
	name := "bouncer-transform-hc-e2e"
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	args := []string{"run", "-d", "--name", name, "--entrypoint", p.Entrypoint[0]}
	for k, v := range p.Environment {
		args = append(args, "-e", k+"="+*v)
	}
	args = append(args, p.Image)
	args = append(args, p.Entrypoint[1:]...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	test := p.HealthCheck.Test // CMD bash -c <script>
	var out []byte
	for i := 0; i < 50; i++ {
		if out, err = exec.Command("docker", append([]string{"exec", name}, test[1:]...)...).CombinedOutput(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("healthcheck never passed: %v\n%s", err, out)
}
