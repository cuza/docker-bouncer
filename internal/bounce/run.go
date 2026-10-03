package bounce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/revision"
)

// ErrFailed: the bounce gave up; the old version keeps serving.
var ErrFailed = errors.New("bounce failed")

// Scaler starts replicas of App until total exist (docker compose up --scale).
type Scaler interface {
	ScaleUp(ctx context.Context, app types.ServiceConfig, total int) error
}

// Runner executes one Service's bounce: observe → Plan → act until Done or Fail.
// App is the stamped desired replica service; N is deploy.replicas.
type Runner struct {
	Project string
	Svc     config.Service
	App     types.ServiceConfig
	N       int
	Engine  engine.Engine
	Proxy   engine.Proxy
	Scaler  Scaler
	Log     func(format string, a ...any)
	Now     func() time.Time

	runStart     time.Time
	healthySince map[string]time.Time
	listed       map[string]bool // nil until the first SetList of this run
}

func (r *Runner) observe(ctx context.Context) (State, error) {
	reps, err := r.Engine.Replicas(ctx, r.Project, r.Svc.Name)
	if err != nil {
		return State{}, err
	}
	health, err := r.Proxy.Health(ctx)
	if err != nil {
		return State{}, err
	}
	now := r.Now()
	st := State{N: r.N, Spec: r.Svc.Spec, Desired: r.App.Labels[revision.LabelSpecHash], Now: now, RunStart: r.runStart}
	for _, rep := range reps {
		o := Observed{Replica: rep, Hash: rep.Labels[revision.LabelSpecHash], EnvoyHealthy: health[rep.Name]}
		if o.EnvoyHealthy && listable(o) {
			if _, ok := r.healthySince[rep.Name]; !ok {
				r.healthySince[rep.Name] = now
			}
			o.HealthySince = r.healthySince[rep.Name]
		} else {
			delete(r.healthySince, rep.Name)
		}
		// Before this run's first SetList the proxy is the truth (every listed
		// host appears in /clusters); afterwards the runner's own list is.
		_, o.Listed = health[rep.Name]
		if r.listed != nil {
			o.Listed = r.listed[rep.Name]
		}
		st.Replicas = append(st.Replicas, o)
	}
	return st, nil
}

// Run loops observe → plan → act until the Service has converged. It returns
// nil when converged and an error wrapping ErrFailed when the bounce failed.
func (r *Runner) Run(ctx context.Context) error {
	r.runStart = r.Now()
	r.healthySince = map[string]time.Time{}
	r.listed = nil
	for {
		st, err := r.observe(ctx)
		if err != nil {
			return err
		}
		step := Plan(st)
		switch step.Kind {
		case Done:
			r.Log("%s: converged (%d replicas)", r.Svc.Name, r.N)
			return nil
		case Wait:
			r.Engine.Wait(ctx, r.Project, time.Second)
		case SetList:
			r.stopDraining(ctx, st, step.List)
			if err := r.setList(ctx, step.List); err != nil {
				return err
			}
		case ScaleUp:
			r.Log("%s: starting replica %d", r.Svc.Name, step.Total)
			if err := r.Scaler.ScaleUp(ctx, r.App, step.Total); err != nil {
				return err
			}
		case Drain:
			if err := r.drain(ctx, st, *step.Replica); err != nil {
				return err
			}
		case Fail:
			r.Log("%s: %s: %s", r.Svc.Name, step.Replica.Name, step.Reason)
			if err := r.remove(ctx, st, *step.Replica); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s: %s %s", ErrFailed, r.Svc.Name, step.Replica.Name, step.Reason)
		}
		if err := ctx.Err(); err != nil { // Ctrl-C: the current step finished
			return err
		}
	}
}

func (r *Runner) setList(ctx context.Context, names []string) error {
	if err := r.Proxy.SetList(ctx, names); err != nil {
		return err
	}
	r.listed = map[string]bool{}
	for _, n := range names {
		r.listed[n] = true
	}
	return nil
}

// stopDraining sends drain_method http's stop_draining to replicas about to be
// re-added to the list that predate this run: an earlier, killed run may have
// drained them. It also reaches replicas that were never drained (the first
// run after the proxy was (re)created, a replica back from a failing Docker
// healthcheck), so the endpoint must be idempotent, as PaaSTA's is.
func (r *Runner) stopDraining(ctx context.Context, st State, added []string) {
	c := r.Svc.Spec.DrainHTTP.StopDraining
	if r.Svc.Spec.DrainMethod != config.DrainHTTP || c == nil {
		return
	}
	for _, o := range st.Replicas {
		if !o.Listed && slices.Contains(added, o.Name) && o.Created.Before(r.runStart) {
			if _, err := r.call(ctx, o, c); err != nil {
				r.Log("%s: stop_draining %s: %v", r.Svc.Name, o.Name, err)
			}
		}
	}
}

// without: the current list minus name.
func without(st State, name string) []string {
	var out []string
	for _, o := range st.Replicas {
		if o.Listed && o.Name != name {
			out = append(out, o.Name)
		}
	}
	return out
}

func (r *Runner) drain(ctx context.Context, st State, o Observed) error {
	r.Log("%s: draining %s", r.Svc.Name, o.Name)
	if o.Listed {
		if err := r.setList(ctx, without(st, o.Name)); err != nil {
			return err
		}
		o.Listed = false
	}
	deadline := r.Now().Add(r.Svc.Spec.DrainDelay)
	switch r.Svc.Spec.DrainMethod {
	case config.DrainEnvoy:
		for o.Running && r.Now().Before(deadline) {
			n, err := r.Proxy.Conns(ctx, o.IPs)
			if err != nil { // transient: keep waiting until the deadline
				r.Log("%s: connections of %s: %v", r.Svc.Name, o.Name, err)
			} else if n == 0 {
				break
			}
			r.Engine.Wait(ctx, r.Project, 500*time.Millisecond)
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	case config.DrainHTTP:
		if o.Running {
			if err := r.httpDrain(ctx, o, deadline); ctx.Err() != nil {
				return ctx.Err()
			} else if err != nil {
				r.Log("%s: http drain of %s: %v (stopping anyway)", r.Svc.Name, o.Name, err)
			}
		}
	}
	return r.remove(ctx, st, o)
}

// call sends one drain_method http request to the replica; ok when the code
// is in success_codes (any 2xx when empty).
func (r *Runner) call(ctx context.Context, o Observed, c *config.HTTPCall) (bool, error) {
	method := c.Method
	if method == "" {
		method = "GET"
	}
	code, err := r.Proxy.HTTP(ctx, method, o.Name, r.Svc.Ports[0].Target, c.Path)
	if err != nil {
		return false, err
	}
	if len(c.SuccessCodes) == 0 {
		return code >= 200 && code < 300, nil
	}
	return slices.Contains(c.SuccessCodes, code), nil
}

func (r *Runner) httpDrain(ctx context.Context, o Observed, deadline time.Time) error {
	if _, err := r.call(ctx, o, r.Svc.Spec.DrainHTTP.Drain); err != nil {
		return err
	}
	for r.Now().Before(deadline) {
		if ok, _ := r.call(ctx, o, r.Svc.Spec.DrainHTTP.IsSafeToKill); ok {
			return nil
		}
		r.Engine.Wait(ctx, r.Project, time.Second)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("not safe to kill after %s", r.Svc.Spec.DrainDelay)
}

// remove takes the replica out of the list, stops it, then removes it.
func (r *Runner) remove(ctx context.Context, st State, o Observed) error {
	if o.Listed {
		if err := r.setList(ctx, without(st, o.Name)); err != nil {
			return err
		}
	}
	if o.Running {
		if err := r.Engine.Stop(ctx, o.ID); err != nil {
			return err
		}
	}
	return r.Engine.Remove(ctx, o.ID)
}
