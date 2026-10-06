package bounce

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/envoy"
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
	r.Svc.Spec.HealthPath, r.Svc.Spec.HealthStrict = "/nope", true
	err := r.Run(context.Background())
	if !errors.Is(err, ErrFailed) || !strings.HasSuffix(err.Error(), "api: new replica proj-api-app-2 never passed GET /nope (expects 2xx) within "+r.Svc.Spec.HealthTimeout.String()+" (last answer: HTTP 200)") {
		t.Fatalf("got %v", err)
	}
	if got := w.hashes(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("old replica must still run, got %v", got)
	}
	if !w.listed("proj-api-app-1") {
		t.Fatal("old replica must still be listed")
	}
}

func TestRunFailRemovesEveryUnhealthyNewReplica(t *testing.T) {
	w := newWorld("new", 3) // app-1 healthy; app-2 and app-3 never healthy
	w.never = map[string]bool{"proj-api-app-2": true, "proj-api-app-3": true}
	w.reps[2].Created = t0.Add(time.Hour) // app-3 is far from its own deadline
	r := w.runner(config.MethodCrossover, 3, "new")
	if err := r.Run(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	if len(w.reps) != 1 || w.reps[0].Name != "proj-api-app-1" || !w.listed("proj-api-app-1") {
		t.Fatalf("only the healthy new replica stays, listed: %v %v", w.reps, w.list)
	}
}

// The last lines of a replica that never turns healthy are saved before it is
// removed, one file each, and the healthy one leaves none.
func TestRunFailSavesLogsOfEveryUnhealthyNewReplicaBeforeRemovingIt(t *testing.T) {
	w := newWorld("new", 3) // app-1 healthy; app-2 and app-3 never healthy
	w.never = map[string]bool{"proj-api-app-2": true, "proj-api-app-3": true}
	w.reps[2].Created = t0.Add(time.Hour)
	w.logs = map[string]string{"proj-api-app-2": "boot\nmissing setting X", "proj-api-app-3": "boot\ncrash\n"}
	dir := filepath.Join(t.TempDir(), "failures")
	r := w.runner(config.MethodCrossover, 3, "new")
	r.FailureLogs = FailureLogs{Dir: dir, Lines: 5}
	if err := r.Run(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	stamp := w.now.UTC().Format("20060102T150405Z")
	for name, want := range map[string]string{"proj-api-app-2": "boot\nmissing setting X\n", "proj-api-app-3": "boot\ncrash\n"} {
		got, err := os.ReadFile(filepath.Join(dir, name+"-"+stamp+".log"))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
		saved, removed := slices.Index(w.events, "logs:"+name+":5"), slices.Index(w.events, "rm:"+name)
		if saved < 0 || removed < 0 || saved > removed {
			t.Fatalf("%s: logs must be read before it is removed: %v", name, w.events)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 2 {
		t.Fatalf("only the unhealthy replicas leave a file, got %v", files)
	}
}

func TestRunFailKeepsDefaultLineCountAndSavesNothingWithoutADir(t *testing.T) {
	w := newWorld("new", 2)
	w.never = map[string]bool{"proj-api-app-2": true}
	r := w.runner(config.MethodCrossover, 2, "new")
	if err := r.Run(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	if slices.ContainsFunc(w.events, func(e string) bool { return strings.HasPrefix(e, "logs:") }) {
		t.Fatalf("no dir, no log read: %v", w.events)
	}

	w = newWorld("new", 2)
	w.never = map[string]bool{"proj-api-app-2": true}
	r = w.runner(config.MethodCrossover, 2, "new")
	r.FailureLogs = FailureLogs{Dir: t.TempDir()}
	if err := r.Run(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	if !slices.Contains(w.events, "logs:proj-api-app-2:200") {
		t.Fatalf("default is %d lines: %v", DefaultFailureLogLines, w.events)
	}
}

// Saving is best effort: whatever goes wrong, the bounce fails as it would have
// and the replica is removed all the same.
func TestRunFailSavingLogsNeverChangesTheOutcome(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, fl := range map[string]struct {
		dir     string
		logsErr error
	}{
		"logs cannot be read":   {dir: t.TempDir(), logsErr: errors.New("no such container")},
		"dir cannot be created": {dir: filepath.Join(file, "sub")},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld("new", 2)
			w.never = map[string]bool{"proj-api-app-2": true}
			w.logsErr = fl.logsErr
			r := w.runner(config.MethodCrossover, 2, "new")
			r.FailureLogs = FailureLogs{Dir: fl.dir}
			if err := r.Run(context.Background()); !errors.Is(err, ErrFailed) {
				t.Fatalf("got %v", err)
			}
			if len(w.reps) != 1 || w.reps[0].Name != "proj-api-app-1" {
				t.Fatalf("the unhealthy replica is removed anyway: %v", w.reps)
			}
			if left, _ := filepath.Glob(filepath.Join(fl.dir, "*")); len(left) != 0 {
				t.Fatalf("nothing is left behind: %v", left)
			}
		})
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

func TestRunBrutalStopsWithoutDraining(t *testing.T) {
	w := newWorld("old", 1)
	w.conns["proj-api-app-1"] = 1 << 30 // would block an envoy drain until drain_delay
	r := w.runner(config.MethodBrutal, 1, "new")
	r.Svc.Spec.DrainHTTP = config.HTTPDrain{Drain: &config.HTTPCall{Method: "POST", Path: "/drain"}}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.hashes(); len(got) != 1 || got[0] != "new" || w.connPolls != 0 || len(w.http) != 0 {
		t.Fatalf("replicas %v conn polls %d http %v", got, w.connPolls, w.http)
	}
	// still taken off the list before it stops
	if w.events[len(w.events)-3] != "list:proj-api-app-2" || w.events[len(w.events)-2] != "stop:proj-api-app-1" {
		t.Fatalf("events %v", w.events)
	}
	// the HTTP drain hooks are skipped too
	w2 := newWorld("old", 1)
	r2 := w2.runner(config.MethodBrutal, 1, "new")
	r2.Svc.Spec.DrainMethod, r2.Svc.Spec.DrainHTTP = config.DrainHTTP, r.Svc.Spec.DrainHTTP
	if err := r2.Run(context.Background()); err != nil || len(w2.http) != 0 {
		t.Fatalf("err %v http %v", err, w2.http)
	}
}

// Settings in the proxy's cluster file are re-applied in place only when they
// differ from what this version renders, keeping the listed hosts.
func TestRunReconcilesClusterSettings(t *testing.T) {
	w := newWorld("new", 2)
	if err := w.runner(config.MethodCrossover, 2, "new").Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.events) != 0 {
		t.Fatalf("identical file must not be rewritten: %v", w.events)
	}

	w = newWorld("new", 2)
	r := w.runner(config.MethodCrossover, 2, "new")
	stale := w.svc
	stale.Spec.HealthPath = "/old"
	w.cds = envoy.Clusters(stale, []string{"proj-api-app-1", "proj-api-app-2"})
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.events, "|") != "list:proj-api-app-1,proj-api-app-2" || w.cds != envoy.Clusters(w.svc, []string{"proj-api-app-1", "proj-api-app-2"}) {
		t.Fatalf("stale settings must be re-applied with the same list: %v", w.events)
	}
}

// A seed file (the replicas' alias) is replaced by the plan's real list, not
// re-applied as is.
func TestRunReplacesSeed(t *testing.T) {
	w := newWorld("new", 1)
	w.list = map[string]bool{}
	w.cds = envoy.SeedClusters(config.Service{Name: "api", Ports: []config.Port{{Target: 8080}}})
	if err := w.runner(config.MethodCrossover, 1, "new").Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.events, "|") != "list:proj-api-app-1" {
		t.Fatalf("events %v", w.events)
	}
}
