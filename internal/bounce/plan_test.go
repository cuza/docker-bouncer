package bounce

import (
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func spec(method string) config.Spec {
	return config.Spec{BounceMethod: method, MarginFactor: 0.95, OverprovisionFactor: 1,
		MinTaskUptime: 10 * time.Second, HealthTimeout: 300 * time.Second, DrainMethod: config.DrainEnvoy}
}

// rep builds a replica: hash "new"/"old", happy = running+envoy healthy for 1 min, listed.
func rep(name, hash string, happy bool) Observed {
	o := Observed{Replica: engine.Replica{ID: name, Name: name, Running: true, Created: t0.Add(-time.Minute)},
		Hash: hash, Listed: true}
	if happy {
		o.EnvoyHealthy, o.HealthySince = true, t0.Add(-time.Minute)
	}
	return o
}

func state(method string, n int, rs ...Observed) State {
	return State{N: n, Spec: spec(method), Desired: "new", Now: t0, Replicas: rs}
}

func TestLimitsRounding(t *testing.T) {
	cases := []struct {
		n              int
		over, margin   float64
		surge, unavail int
	}{
		{1, 1, 0.95, 1, 0}, {2, 1, 0.95, 2, 0}, {20, 1, 0.95, 20, 1}, {100, 0.25, 0.95, 25, 5},
		{3, 0.5, 0.5, 2, 1}, {4, 0, 1, 0, 0},
	}
	for _, c := range cases {
		s := config.Spec{OverprovisionFactor: c.over, MarginFactor: c.margin}
		if su, un := Limits(c.n, s); su != c.surge || un != c.unavail {
			t.Errorf("n=%d over=%v margin=%v: got %d/%d want %d/%d", c.n, c.over, c.margin, su, un, c.surge, c.unavail)
		}
	}
	// crossover must always be able to make progress
	if su, un := Limits(4, config.Spec{BounceMethod: config.MethodCrossover, MarginFactor: 1}); su != 1 || un != 0 {
		t.Errorf("crossover 0/0: got %d/%d want 1/0", su, un)
	}
}

func TestNothingToDo(t *testing.T) {
	if s := Plan(state(config.MethodCrossover, 1, rep("a", "new", true))); s.Kind != Done {
		t.Fatalf("got %+v", s)
	}
}

func TestCrossoverScalesUpFirst(t *testing.T) {
	s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true)))
	if s.Kind != ScaleUp || s.Total != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestCrossoverWaitsForHappiness(t *testing.T) {
	young := rep("b", "new", true)
	young.HealthySince = t0.Add(-5 * time.Second) // under min_task_uptime
	s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), young))
	if s.Kind != Wait {
		t.Fatalf("got %+v", s)
	}
}

func TestCrossoverDrainsOldOnceNewIsHappy(t *testing.T) {
	s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), rep("b", "new", true)))
	if s.Kind != Drain || s.Replica.Name != "a" {
		t.Fatalf("got %+v", s)
	}
}

func TestPlanRelistsUnlistedHealthyReplica(t *testing.T) {
	half := rep("a", "old", true)
	half.Listed = false // Bouncer died after removing it from the list
	s := Plan(state(config.MethodCrossover, 1, half))
	if s.Kind != SetList || len(s.List) != 1 || s.List[0] != "a" {
		t.Fatalf("got %+v", s)
	}
}

func TestNewReplicaIsListedWhenRunning(t *testing.T) {
	b := rep("b", "new", false)
	b.Listed = false
	s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b))
	if s.Kind != SetList || len(s.List) != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestDockerUnhealthyReplicaIsNotListed(t *testing.T) {
	b := rep("b", "new", false)
	b.Listed, b.DockerHealth = false, "starting"
	if s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b)); s.Kind == SetList {
		t.Fatalf("a starting replica must not be listed: %+v", s)
	}
}

func TestPlanDrainsDeadOldFirst(t *testing.T) {
	dead := rep("a", "old", false)
	dead.Running = false
	s := Plan(state(config.MethodCrossover, 2, dead, rep("b", "old", true)))
	if s.Kind != Drain || s.Replica.Name != "a" {
		t.Fatalf("got %+v", s)
	}
}

func TestFailsAfterHealthTimeout(t *testing.T) {
	b := rep("b", "new", false)
	b.Created = t0.Add(-301 * time.Second)
	s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b))
	if s.Kind != Fail || s.Replica.Name != "b" {
		t.Fatalf("got %+v", s)
	}
}

func TestUpThenDownWaitsForAllNew(t *testing.T) {
	st := state(config.MethodUpThenDown, 2, rep("a", "old", true), rep("b", "old", true), rep("c", "new", true))
	if s := Plan(st); s.Kind != ScaleUp || s.Total != 4 {
		t.Fatalf("got %+v", s)
	}
	st.Replicas = append(st.Replicas, rep("d", "new", false))
	if s := Plan(st); s.Kind != Wait {
		t.Fatalf("must not drain before every new replica is happy: %+v", s)
	}
}

func TestDownThenUpDrainsBeforeStarting(t *testing.T) {
	s := Plan(state(config.MethodDownThenUp, 1, rep("a", "old", true)))
	if s.Kind != Drain {
		t.Fatalf("got %+v", s)
	}
	if s := Plan(state(config.MethodDownThenUp, 1)); s.Kind != ScaleUp || s.Total != 1 {
		t.Fatalf("got %+v", s)
	}
}

func TestBrutalIgnoresHealth(t *testing.T) {
	st := state(config.MethodBrutal, 1, rep("a", "old", true), rep("b", "new", false))
	if s := Plan(st); s.Kind != Drain || s.Replica.Name != "a" {
		t.Fatalf("got %+v", s)
	}
}

func TestExtraNewReplicaIsTrimmed(t *testing.T) {
	s := Plan(state(config.MethodCrossover, 1, rep("a", "new", true), rep("b", "new", false)))
	if s.Kind != Drain || s.Replica.Name != "b" {
		t.Fatalf("got %+v", s)
	}
}

func TestMarginAllowsDrainingBeforeReplacement(t *testing.T) {
	st := state(config.MethodCrossover, 2, rep("a", "old", true), rep("b", "old", true))
	st.Spec.MarginFactor = 0.5      // unavailable = 1
	st.Spec.OverprovisionFactor = 0 // surge = 0: must make room first
	if s := Plan(st); s.Kind != Drain {
		t.Fatalf("got %+v", s)
	}
}

func TestUnservingOldDoesNotBlockCrossover(t *testing.T) {
	a := rep("a", "old", false)
	a.Listed, a.DockerHealth = false, "unhealthy" // never listed, so never "not serving" by rule 2
	s := Plan(state(config.MethodCrossover, 1, a, rep("b", "new", true)))
	if s.Kind != Drain || s.Replica.Name != "a" {
		t.Fatalf("got %+v", s)
	}
}

func TestTwoOldOneUnservingDoesNotDeadlock(t *testing.T) {
	b := rep("b", "old", false)
	b.Listed, b.DockerHealth = false, "unhealthy"
	st := state(config.MethodCrossover, 2, rep("a", "old", true), b, rep("c", "new", true))
	st.Spec.OverprovisionFactor = 0.5 // surge 1, unavailable 0
	if s := Plan(st); s.Kind != Drain || s.Replica.Name != "b" {
		t.Fatalf("got %+v", s)
	}
}

func TestConvergedServiceIsDone(t *testing.T) {
	a := rep("a", "new", true)
	a.Created, a.Started, a.HealthySince = t0.Add(-time.Hour), t0.Add(-time.Hour), t0 // runner just reset HealthySince
	if s := Plan(state(config.MethodCrossover, 1, a)); s.Kind != Done {
		t.Fatalf("got %+v", s)
	}
}

func TestNeverHealthyFailsFromStartTime(t *testing.T) {
	b := rep("b", "new", false)
	b.Created, b.Started = t0.Add(-time.Hour), t0.Add(-301*time.Second)
	if s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b)); s.Kind != Fail || s.Replica.Name != "b" {
		t.Fatalf("got %+v", s)
	}
	b.Started = t0.Add(-299 * time.Second)
	if s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b)); s.Kind == Fail {
		t.Fatalf("deadline counts from Started, not Created: %+v", s)
	}
}

func TestHealthyUnderUptimeDoesNotFail(t *testing.T) {
	b := rep("b", "new", true)
	b.Started, b.HealthySince = t0.Add(-301*time.Second), t0.Add(-5*time.Second)
	if s := Plan(state(config.MethodCrossover, 1, rep("a", "old", true), b)); s.Kind != Wait {
		t.Fatalf("got %+v", s)
	}
}

func TestRebootDoesNotDrainBelowCapacity(t *testing.T) {
	var olds []Observed
	for _, n := range []string{"a", "b"} {
		o := rep(n, "old", false) // running, listed, Envoy not healthy yet
		o.Created, o.Started = t0.Add(-time.Hour), t0.Add(-2*time.Second)
		olds = append(olds, o)
	}
	if s := Plan(state(config.MethodCrossover, 2, olds...)); s.Kind != ScaleUp {
		t.Fatalf("got %+v", s)
	}
}

func TestFlappingOldIsKeptWhileNewWarms(t *testing.T) {
	a := rep("a", "old", false) // flapped to Envoy-unhealthy
	a.Created, a.Started = t0.Add(-time.Hour), t0.Add(-time.Hour)
	b := rep("b", "new", true)
	b.HealthySince = t0.Add(-5 * time.Second)
	if s := Plan(state(config.MethodCrossover, 1, a, b)); s.Kind == Drain {
		t.Fatalf("draining a leaves zero serving: %+v", s)
	}
}

func TestDeadlineCountsFromRunStart(t *testing.T) {
	b := rep("b", "new", false) // Envoy-unhealthy, e.g. proxy just restarted
	b.Created, b.Started = t0.Add(-2*time.Hour), t0.Add(-2*time.Hour)
	st := state(config.MethodCrossover, 1, b)
	st.RunStart = t0.Add(-10 * time.Second)
	if s := Plan(st); s.Kind != Wait {
		t.Fatalf("got %+v", s)
	}
	st.RunStart = t0.Add(-301 * time.Second)
	if s := Plan(st); s.Kind != Fail || s.Replica.Name != "b" {
		t.Fatalf("got %+v", s)
	}
}
