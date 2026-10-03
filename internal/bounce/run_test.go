package bounce

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
)

func TestRunCrossoverReplacesOldReplica(t *testing.T) {
	w := newWorld("old", 1)
	r := w.runner(config.MethodCrossover, 1, "new")
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.hashes(); len(got) != 1 || got[0] != "new" {
		t.Fatalf("replicas %v", got)
	}
	// the old replica was removed from the list before it was stopped
	if w.events[len(w.events)-3] != "list:proj-api-app-2" || w.events[len(w.events)-2] != "stop:proj-api-app-1" {
		t.Fatalf("events %v", w.events)
	}
}

func TestRunDrainWaitsForConnections(t *testing.T) {
	w := newWorld("old", 1)
	w.conns["proj-api-app-1"] = 2 // two in-flight requests, finishing one per poll
	r := w.runner(config.MethodCrossover, 1, "new")
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.connPolls < 3 {
		t.Fatalf("drain did not wait for connections (%d polls)", w.connPolls)
	}
}

func TestRunDrainGivesUpAtDelay(t *testing.T) {
	w := newWorld("old", 1)
	w.conns["proj-api-app-1"] = 1 << 30 // never finishes
	r := w.runner(config.MethodCrossover, 1, "new")
	r.Svc.Spec.DrainDelay = 3 * time.Second
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.hashes(); len(got) != 1 || got[0] != "new" || len(w.stopped) != 1 || w.stopped[0] != "proj-api-app-1" {
		t.Fatalf("replicas %v stopped %v", got, w.stopped)
	}
	if w.connPolls > 2 {
		t.Fatalf("drain polled %d times past a 3s delay", w.connPolls)
	}
}

func TestRunDrainSurvivesConnsError(t *testing.T) {
	w := newWorld("old", 1)
	w.connErrs = 1
	w.conns["proj-api-app-1"] = 2
	r := w.runner(config.MethodCrossover, 1, "new")
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.connPolls != 4 { // error, 2, 1, 0
		t.Fatalf("drain stopped waiting after %d polls", w.connPolls)
	}
}

func httpRunner(w *world) *Runner {
	r := w.runner(config.MethodCrossover, 1, "new")
	r.Svc.Spec.DrainMethod = config.DrainHTTP
	r.Svc.Spec.DrainHTTP = config.HTTPDrain{Drain: &config.HTTPCall{Method: "POST", Path: "/drain"},
		IsSafeToKill: &config.HTTPCall{Path: "/safe"}}
	return r
}

func TestRunHTTPDrainPollsUntilSafe(t *testing.T) {
	w := newWorld("old", 1)
	w.unsafe = 2
	if err := httpRunner(w).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := "proj-api-app-1"
	want := []string{"POST " + h + " /drain", "GET " + h + " /safe", "GET " + h + " /safe", "GET " + h + " /safe"}
	if strings.Join(w.http, "|") != strings.Join(want, "|") {
		t.Fatalf("http %v", w.http)
	}
	if w.events[len(w.events)-2] != "stop:"+h || w.events[len(w.events)-1] != "rm:"+h {
		t.Fatalf("events %v", w.events)
	}
}

func TestRunHTTPDrainGivesUpAtDelay(t *testing.T) {
	w := newWorld("old", 1)
	w.unsafe = 1 << 30
	if err := httpRunner(w).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.stopped) != 1 || w.stopped[0] != "proj-api-app-1" || len(w.http) > 14 {
		t.Fatalf("stopped %v after %d calls", w.stopped, len(w.http))
	}
}

func TestRunHTTPDrainStopsOnCancel(t *testing.T) {
	w := newWorld("old", 1)
	w.unsafe = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	w.onSafe = cancel
	err := httpRunner(w).Run(ctx)
	if !errors.Is(err, context.Canceled) || len(w.http) != 2 || len(w.stopped) != 0 {
		t.Fatalf("err %v http %v stopped %v", err, w.http, w.stopped)
	}
}

func TestRunFailsAndKeepsOldServing(t *testing.T) {
	w := newWorld("old", 1)
	w.newHealthy = false // new replicas never pass Envoy's check
	r := w.runner(config.MethodCrossover, 1, "new")
	err := r.Run(context.Background())
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	if got := w.hashes(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("old replica must still run, got %v", got)
	}
	if !w.listed("proj-api-app-1") {
		t.Fatal("old replica must still be listed")
	}
}

func TestRunNoopWhenConverged(t *testing.T) {
	w := newWorld("new", 1)
	r := w.runner(config.MethodCrossover, 1, "new")
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.scaled != 0 || len(w.stopped) != 0 {
		t.Fatalf("scaled %d stopped %v", w.scaled, w.stopped)
	}
}

// A replica created long ago that is briefly unhealthy when the run starts
// gets a full bounce_health_timeout from the run's start, not from Created.
func TestRunHealthDeadlineCountsFromRunStart(t *testing.T) {
	w := newWorld("new", 1)
	w.reps[0].Created = t0.Add(-time.Hour)
	w.sick = 2
	r := w.runner(config.MethodCrossover, 1, "new")
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.scaled != 0 || len(w.stopped) != 0 || w.now == t0 {
		t.Fatalf("scaled %d stopped %v now %v", w.scaled, w.stopped, w.now)
	}
}

// A replica a killed run had drained over HTTP gets stop_draining when re-added.
func TestRunStopDrainingOnReAdd(t *testing.T) {
	w := newWorld("new", 1)
	w.list = map[string]bool{} // killed mid-drain: unlisted
	w.reps[0].Created = t0.Add(-time.Hour)
	r := w.runner(config.MethodCrossover, 1, "new")
	r.Svc.Spec.DrainMethod = config.DrainHTTP
	r.Svc.Spec.DrainHTTP = config.HTTPDrain{StopDraining: &config.HTTPCall{Method: "POST", Path: "/undrain"}}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.listed("proj-api-app-1") || len(w.http) != 1 || w.http[0] != "POST proj-api-app-1 /undrain" {
		t.Fatalf("listed %v http %v", w.list, w.http)
	}
}
