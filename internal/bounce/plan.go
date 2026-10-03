// Package bounce decides and executes one Service's bounce.
package bounce

import (
	"math"
	"sort"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
)

// Observed is one replica as the runner sees it: Docker state plus its spec
// hash, Envoy health and whether it is in the proxy's endpoint list.
type Observed struct {
	engine.Replica
	Hash         string
	EnvoyHealthy bool
	HealthySince time.Time // zero = not healthy yet in this run
	Listed       bool
}

// State is everything Plan needs: N is deploy.replicas, Desired the spec hash
// new replicas carry.
type State struct {
	N        int
	Spec     config.Spec
	Desired  string
	Now      time.Time
	Replicas []Observed
}

// Kind is what a Step asks the runner to do.
type Kind int

const (
	Wait Kind = iota
	Done
	SetList
	ScaleUp
	Drain
	Fail
)

// Step is one action. Replica is set for Drain and Fail, List for SetList,
// Total (the replica count to scale to) for ScaleUp.
type Step struct {
	Kind    Kind
	Replica *Observed
	List    []string
	Total   int
	Reason  string
}

const eps = 1e-9

// Limits returns how many replicas above N may run (surge) and how many of N
// may be missing (unavailable) during a bounce of method s.BounceMethod.
func Limits(n int, s config.Spec) (surge, unavailable int) {
	surge = int(math.Ceil(float64(n)*s.OverprovisionFactor - eps))
	unavailable = int(math.Floor(float64(n)*(1-s.MarginFactor) + eps))
	switch s.BounceMethod {
	case config.MethodUpThenDown:
		surge, unavailable = n, 0
	case config.MethodDownThenUp:
		surge, unavailable = 0, n
	case config.MethodBrutal:
		surge, unavailable = n, n
	case config.MethodCrossover:
		if surge == 0 && unavailable == 0 { // neither room to add nor to remove: allow one extra
			surge = 1
		}
	}
	return surge, unavailable
}

// healthy: serving now. Brutal ignores health and only needs it running.
func (s State) healthy(r Observed) bool {
	if s.Spec.BounceMethod == config.MethodBrutal {
		return r.Running
	}
	return listable(r) && r.EnvoyHealthy
}

// happy: healthy for at least min_task_uptime; gates draining old replicas.
func (s State) happy(r Observed) bool {
	if s.Spec.BounceMethod == config.MethodBrutal {
		return r.Running
	}
	return s.healthy(r) && !r.HealthySince.IsZero() && s.Now.Sub(r.HealthySince) >= s.Spec.MinTaskUptime
}

// serving: counts towards capacity when kept.
func serving(r Observed) bool { return r.Running && r.EnvoyHealthy }

// listable: running and not failing a Docker healthcheck it has.
func listable(r Observed) bool {
	return r.Running && (r.DockerHealth == "" || r.DockerHealth == "healthy")
}

// Plan returns the next step of a bounce. It is pure: the runner observes,
// applies the step and calls Plan again until Done or Fail.
func Plan(s State) Step {
	var old, cur []Observed
	for _, r := range s.Replicas {
		if r.Hash == s.Desired {
			cur = append(cur, r)
		} else {
			old = append(old, r)
		}
	}
	sortReplicas(old)
	sortReplicas(cur)
	// unserving old replicas drain first: they cost no capacity
	sort.SliceStable(old, func(i, j int) bool { return !serving(old[i]) && serving(old[j]) })

	// 1. A new replica that never became healthy within bounce_health_timeout
	//    of its start fails the bounce.
	for i := range cur {
		r := cur[i]
		start := r.Started
		if start.IsZero() {
			start = r.Created
		}
		if !s.healthy(r) && r.HealthySince.IsZero() && s.Now.Sub(start) > s.Spec.HealthTimeout {
			return Step{Kind: Fail, Replica: &r, Reason: "not healthy within bounce_health_timeout"}
		}
	}

	// 2. Dead old replicas go at once, whatever the limits. Running but
	//    Envoy-unhealthy ones are left to rule 4 (they may be warming up
	//    after a reboot or flapping; rule 4 drains them at no capacity cost).
	for i := range old {
		r := old[i]
		if !r.Running {
			return Step{Kind: Drain, Replica: &r, Reason: "old replica is not serving"}
		}
	}

	// 3. The list must hold exactly the listable replicas (re-adds a
	//    half-drained one after a killed run, adds new ones as they start).
	var want []string
	changed := false
	for _, r := range s.Replicas {
		l := listable(r)
		if l {
			want = append(want, r.Name)
		}
		if l != r.Listed {
			changed = true
		}
	}
	if changed {
		sort.Strings(want)
		return Step{Kind: SetList, List: want}
	}

	happyNew, healthyNew := 0, 0
	for _, r := range cur {
		if s.happy(r) {
			happyNew++
		}
		if s.healthy(r) {
			healthyNew++
		}
	}
	liveOld := 0
	for _, r := range old {
		if serving(r) {
			liveOld++
		}
	}

	if len(old) == 0 {
		if len(cur) > s.N { // after a resumed run: trim the least healthy
			r := cur[len(cur)-1]
			for i := len(cur) - 1; i >= 0; i-- {
				if !s.healthy(cur[i]) {
					r = cur[i]
					break
				}
			}
			return Step{Kind: Drain, Replica: &r, Reason: "more replicas than deploy.replicas"}
		}
		if len(cur) < s.N {
			return Step{Kind: ScaleUp, Total: len(cur) + 1}
		}
		if healthyNew == s.N { // min_task_uptime only gates draining old replicas
			return Step{Kind: Done}
		}
		return Step{Kind: Wait, Reason: "waiting for new replicas to become healthy"}
	}

	surge, unavailable := Limits(s.N, s.Spec)

	// 4. Drain an old replica when capacity allows (unserving, then oldest
	//    first). Brutal always may: it drains every old replica before
	//    scaling up, like downthenup without health gates.
	r := old[0]
	lost := 0 // draining a replica that serves nothing costs no capacity
	if serving(r) {
		lost = 1
	}
	canDrain := happyNew+liveOld-lost >= s.N-unavailable
	if s.Spec.BounceMethod == config.MethodUpThenDown {
		canDrain = happyNew >= s.N
	}
	if canDrain {
		return Step{Kind: Drain, Replica: &r}
	}

	// 5. Start a new replica when the surge allows.
	if len(cur) < s.N && len(old)+len(cur) < s.N+surge {
		return Step{Kind: ScaleUp, Total: len(old) + len(cur) + 1}
	}
	return Step{Kind: Wait, Reason: "waiting for new replicas to become healthy"}
}

// sortReplicas: oldest container first.
func sortReplicas(rs []Observed) {
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].Created.Equal(rs[j].Created) {
			return rs[i].Created.Before(rs[j].Created)
		}
		return rs[i].Name < rs[j].Name
	})
}
