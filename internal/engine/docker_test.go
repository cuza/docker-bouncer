package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// downClient: the daemon is unreachable, Events fails at once.
type downClient struct{ client.APIClient }

func (downClient) Events(context.Context, client.EventsListOptions) client.EventsResult {
	errs := make(chan error, 1)
	errs <- errors.New("cannot connect to the Docker daemon")
	return client.EventsResult{Err: errs}
}

func TestWaitSleepsWhenEventsFail(t *testing.T) {
	start := time.Now()
	NewDocker(downClient{}).Wait(context.Background(), "proj", 100*time.Millisecond)
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Fatalf("Wait returned after %v, want ≈100ms", d)
	}
}
