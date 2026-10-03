//go:build e2e

package lock

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

func TestDockerLock(t *testing.T) {
	ctx := context.Background()
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	const img = "envoyproxy/envoy:v1.39.1"
	if out, err := exec.Command("docker", "pull", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "bouncer-lock-e2e-bouncer-lock").Run() })
	l := NewDocker(c)

	rel, err := Acquire(ctx, l, "bouncer-lock-e2e", img, "alice", time.Hour, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(ctx, l, "bouncer-lock-e2e", img, "bob", time.Hour, false, time.Now())
	var held ErrHeld
	if !errors.As(err, &held) || held.Owner != "alice" || held.Since.IsZero() {
		t.Fatalf("got %v", err)
	}
	if err := rel(ctx); err != nil {
		t.Fatal(err)
	}
	rel2, err := Acquire(ctx, l, "bouncer-lock-e2e", img, "bob", time.Hour, false, time.Now())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2(ctx)

	if _, err := Acquire(ctx, l, "bouncer-lock-e2e", "bouncer-missing/image:none", "x", time.Hour, false, time.Now()); err == nil || !strings.Contains(err.Error(), "not present locally") {
		t.Fatalf("missing image: %v", err)
	}
}
