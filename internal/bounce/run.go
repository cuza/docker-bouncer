package bounce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/envoy"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/docker/compose/v5/pkg/api"
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
	Events  api.EventProcessor
	Now     func() time.Time

	runStart     time.Time
	healthySince map[string]time.Time
	listed       map[string]bool // nil until the first SetList of this run
	unhealthy    map[string]bool // listed by this run, not reported Healthy yet
}

// event reports a step on the Service's row ("Service web") or a replica's
// row ("Container proj-web-app-1", the row Compose created it on).
func (r *Runner) event(id string, status api.EventStatus, text string, details ...string) {
	r.Events.On(api.Resource{ID: id, Status: status, Text: text, Details: strings.Join(details, " ")})
}

func container(name string) string { return "Container " + name }

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
	r.unhealthy = map[string]bool{}
	svc, progress := "Service "+r.Svc.Name, ""
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	for {
		st, err := r.observe(ctx)
		if err != nil {
			return err
		}
		k := 0
		for _, o := range st.Replicas {
			if o.Hash == st.Desired && st.healthy(o) {
				k++
				if r.unhealthy[o.Name] {
					delete(r.unhealthy, o.Name)
					r.event(container(o.Name), api.Done, api.StatusHealthy)
				}
			}
		}
		step := Plan(st)
		if p := fmt.Sprintf("Bouncing (%d/%d)", k, r.N); p != progress && step.Kind != Done && step.Kind != Fail {
			progress = p
			r.event(svc, api.Working, p)
		}
		switch step.Kind {
		case Done:
			r.event(svc, api.Done, "Converged", fmt.Sprintf("%d replicas", r.N))
			return nil
		case Wait:
			r.Engine.Wait(ctx, r.Project, time.Second)
		case SetList:
			r.stopDraining(ctx, st, step.List)
			if err := r.setList(ctx, step.List); err != nil {
				return err
			}
			for _, o := range st.Replicas {
				if !o.Listed && slices.Contains(step.List, o.Name) {
					r.unhealthy[o.Name] = true
					r.event(container(o.Name), api.Working, "Listed")
				}
			}
		case ScaleUp: // Compose reports Creating/Starting on the new replica's row
			if err := r.Scaler.ScaleUp(ctx, r.App, step.Total); err != nil {
				return err
			}
		case Drain:
			if err := r.drain(ctx, st, *step.Replica); err != nil {
				return err
			}
		case Fail:
			reason := "new replica " + step.Replica.Name + " " + step.Reason + r.lastAnswer(ctx, *step.Replica)
			r.event(svc, api.Error, "Failed: "+reason)
			if err := r.removeUnhealthyNew(ctx, st); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s: %s", ErrFailed, r.Svc.Name, reason)
		}
		if err := ctx.Err(); err != nil { // Ctrl-C: the current step finished
			return err
		}
	}
}

// lastAnswer probes the health path once more for the failure message, e.g.
// " (last answer: HTTP 404)"; Envoy's /clusters keeps only a failed flag.
func (r *Runner) lastAnswer(ctx context.Context, o Observed) string {
	switch {
	case !o.Running:
		return " (not running)"
	case o.DockerHealth != "" && o.DockerHealth != "healthy":
		return " (Docker healthcheck: " + o.DockerHealth + ")"
	}
	type answer struct {
		code int
		err  error
	}
	ch := make(chan answer, 1) // a hung replica must not hang the failure
	go func() {
		code, err := r.Proxy.HTTP(ctx, "GET", o.Name, r.Svc.Ports[0].Target, r.Svc.Spec.HealthPath)
		ch <- answer{code, err}
	}()
	select {
	case a := <-ch:
		if a.err != nil {
			return " (last try: " + a.err.Error() + ")"
		}
		return fmt.Sprintf(" (last answer: HTTP %d)", a.code)
	case <-time.After(5 * time.Second):
		return " (last try: no answer within 5s)"
	}
}

// reconcile rewrites the cluster file with the current list when its settings
// differ from what this version renders (health config changed, or an older
// Bouncer wrote it), so Envoy hot-reloads them instead of the proxy being
// recreated. A seed (the replicas' alias) is left to the plan, which lists the
// real replicas; an unreadable file too.
func (r *Runner) reconcile(ctx context.Context) error {
	cur, err := r.Proxy.Current(ctx)
	if err != nil {
		return err
	}
	hosts, err := envoy.Hostnames(cur)
	if err != nil || slices.Equal(hosts, []string{envoy.SeedHost(r.Svc)}) || cur == envoy.Clusters(r.Svc, hosts) {
		return nil
	}
	if err := r.Proxy.SetList(ctx, hosts); err != nil {
		return err
	}
	r.event("Service "+r.Svc.Name, api.Working, "Proxy config updated")
	return nil
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
				r.event(container(o.Name), api.Warning, "stop_draining failed:", err.Error())
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
	start := r.Now()
	r.event(container(o.Name), api.Working, "Draining")
	if o.Listed {
		if err := r.setList(ctx, without(st, o.Name)); err != nil {
			return err
		}
		o.Listed = false
	}
	deadline := r.Now().Add(r.Svc.Spec.DrainDelay)
	method := r.Svc.Spec.DrainMethod
	if r.Svc.Spec.BounceMethod == config.MethodBrutal {
		method = "" // brutal: off the list, then stop at once (no drain wait)
	}
	switch method {
	case config.DrainEnvoy:
		for o.Running && r.Now().Before(deadline) {
			n, err := r.Proxy.Conns(ctx, o.IPs)
			if err == nil && n == 0 {
				break
			}
			if err != nil && ctx.Err() == nil { // transient: keep waiting until the deadline
				r.event(container(o.Name), api.Working, "Draining", "connections:", err.Error())
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
				r.event(container(o.Name), api.Warning, "Draining", err.Error(), "(stopping anyway)")
			}
		}
	}
	return r.remove(ctx, st, o, fmt.Sprintf("drained %s", r.Now().Sub(start).Round(100*time.Millisecond)))
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

// removeUnhealthyNew removes every new replica that is not healthy, so a
// failed bounce leaves no half-started version behind; healthy new ones stay.
func (r *Runner) removeUnhealthyNew(ctx context.Context, st State) error {
	var bad []Observed
	listed := false
	for _, o := range st.Replicas {
		if o.Hash == st.Desired && !st.healthy(o) {
			bad = append(bad, o)
			listed = listed || o.Listed
		}
	}
	if listed {
		var keep []string
		for _, o := range st.Replicas {
			if o.Listed && !slices.ContainsFunc(bad, func(b Observed) bool { return b.Name == o.Name }) {
				keep = append(keep, o.Name)
			}
		}
		if err := r.setList(ctx, keep); err != nil {
			return err
		}
	}
	for _, o := range bad {
		o.Listed = false // already out of the list
		if err := r.remove(ctx, st, o, "unhealthy"); err != nil {
			return err
		}
	}
	return nil
}

// remove takes the replica out of the list, stops it, then removes it; why
// goes on the Stopped event.
func (r *Runner) remove(ctx context.Context, st State, o Observed, why string) error {
	if o.Listed {
		if err := r.setList(ctx, without(st, o.Name)); err != nil {
			return err
		}
	}
	if o.Running {
		if err := r.Engine.Stop(ctx, o.ID); err != nil {
			return err
		}
		r.event(container(o.Name), api.Done, api.StatusStopped, why)
	}
	if err := r.Engine.Remove(ctx, o.ID); err != nil {
		return err
	}
	r.event(container(o.Name), api.Done, api.StatusRemoved)
	return nil
}
