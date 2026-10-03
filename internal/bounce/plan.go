// Package bounce decides and executes one Service's bounce.
package bounce

import (
	"math"
	"sort"
	"time"

	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
)

type Observed struct {
	engine.Replica
	Hash         string
	EnvoyHealthy bool
	HealthySince time.Time
	Listed       bool
}

type State struct {
	N        int
	Spec     config.Spec
	Desired  string
	Now      time.Time
	Replicas []Observed
}

type Kind int

const (
	Wait Kind = iota
	Done
	SetList
	ScaleUp
	Drain
	Fail
)

type Step struct {
	Kind    Kind
	Replica *Observed
	List    []string
	Total   int
	Reason  string
}

const eps = 1e-9

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
	}
	return surge, unavailable
}

func (s State) happy(r Observed) bool {
	if !r.Running {
		return false
	}
	if s.Spec.BounceMethod == config.MethodBrutal {
		return true
	}
	if r.DockerHealth != "" && r.DockerHealth != "healthy" {
		return false
	}
	return r.EnvoyHealthy && !r.HealthySince.IsZero() && s.Now.Sub(r.HealthySince) >= s.Spec.MinTaskUptime
}

// listable: running and not failing a Docker healthcheck it has.
func listable(r Observed) bool {
	return r.Running && (r.DockerHealth == "" || r.DockerHealth == "healthy")
}

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

	// 1. A new replica past its deadline fails the bounce.
	for i := range cur {
		r := cur[i]
		if !s.happy(r) && s.Spec.BounceMethod != config.MethodBrutal && s.Now.Sub(r.Created) > s.Spec.HealthTimeout {
			return Step{Kind: Fail, Replica: &r, Reason: "not healthy within bounce_health_timeout"}
		}
	}

	// 2. Old replicas that serve nothing go first, whatever the limits
	//    (a crashed or unhealthy old replica must never block a bounce).
	for i := range old {
		r := old[i]
		if !r.Running || (!r.EnvoyHealthy && r.Listed && s.Spec.BounceMethod != config.MethodBrutal && !s.Now.Before(r.Created.Add(s.Spec.HealthTimeout))) {
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

	happyNew := 0
	for _, r := range cur {
		if s.happy(r) {
			happyNew++
		}
	}
	liveOld := 0
	for _, r := range old {
		if r.Running && r.EnvoyHealthy {
			liveOld++
		}
	}

	if len(old) == 0 {
		if len(cur) > s.N { // after a resumed run: trim the least healthy
			r := cur[len(cur)-1]
			for i := len(cur) - 1; i >= 0; i-- {
				if !s.happy(cur[i]) {
					r = cur[i]
					break
				}
			}
			return Step{Kind: Drain, Replica: &r, Reason: "more replicas than deploy.replicas"}
		}
		if len(cur) < s.N {
			return Step{Kind: ScaleUp, Total: len(cur) + 1}
		}
		if happyNew == s.N {
			return Step{Kind: Done}
		}
		return Step{Kind: Wait, Reason: "waiting for new replicas to become healthy"}
	}

	surge, unavailable := Limits(s.N, s.Spec)

	// 4. Drain an old replica when capacity allows (oldest first).
	r := old[0]
	lost := 0 // draining a replica that serves nothing costs no capacity
	if r.Running && r.EnvoyHealthy {
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
