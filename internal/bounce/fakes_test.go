package bounce

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
	"github.com/cuza/docker-bouncer/internal/envoy"
	"github.com/cuza/docker-bouncer/internal/revision"
	"github.com/docker/compose/v5/cmd/display"
)

type world struct {
	now        time.Time
	reps       []engine.Replica
	list       map[string]bool
	conns      map[string]int
	connPolls  int
	newHealthy bool
	never      map[string]bool // replicas Envoy never reports healthy
	sick       int             // the next sick Health calls report every host unhealthy
	desired    string
	scaled     int
	stopped    []string
	events     []string
	http       []string // "METHOD host path" of each HTTP call
	connErrs   int      // the next connErrs Conns calls fail
	unsafe     int      // the next unsafe calls to /safe answer 503
	onSafe     func()   // called on each /safe call
	next       int
	cds        string // the proxy's cluster file
	svc        config.Service
}

// newWorld: n running replicas with hash h, all listed (as after a previous up).
func newWorld(h string, n int) *world {
	w := &world{now: t0, list: map[string]bool{}, conns: map[string]int{}, newHealthy: true}
	for i := 0; i < n; i++ {
		w.add(h)
	}
	for _, r := range w.reps {
		w.list[r.Name] = true
	}
	return w
}

func (w *world) add(hash string) {
	w.next++
	name := fmt.Sprintf("proj-api-app-%d", w.next)
	w.reps = append(w.reps, engine.Replica{ID: name, Name: name, Running: true, Created: w.now, Started: w.now,
		IPs: []string{fmt.Sprintf("10.0.0.%d", w.next)}, Labels: map[string]string{revision.LabelSpecHash: hash}})
}

func (w *world) runner(method string, n int, desired string) *Runner {
	w.desired = desired
	sp := spec(method)
	sp.DrainDelay = 60 * time.Second
	app := types.ServiceConfig{Name: "api-app"}
	app.Labels = types.Labels{revision.LabelSpecHash: desired}
	w.svc = config.Service{Name: "api", Spec: sp, Ports: []config.Port{{Target: 8080}}}
	if w.cds == "" { // as the previous up left it
		var names []string
		for n := range w.list {
			names = append(names, n)
		}
		w.cds = envoy.Clusters(w.svc, names)
	}
	return &Runner{
		Project: "proj", N: n, Engine: w, Proxy: w, Scaler: w,
		Svc:    w.svc,
		App:    app,
		Events: display.Quiet(),
		Now:    func() time.Time { return w.now },
	}
}

func (w *world) hashes() []string {
	var out []string
	for _, r := range w.reps {
		out = append(out, r.Labels[revision.LabelSpecHash])
	}
	return out
}

func (w *world) listed(name string) bool { return w.list[name] }

// engine.Engine
func (w *world) Replicas(context.Context, string, string) ([]engine.Replica, error) {
	return append([]engine.Replica(nil), w.reps...), nil
}
func (w *world) Container(context.Context, string, map[string]string) (*engine.Replica, error) {
	return nil, nil
}
func (w *world) Stop(_ context.Context, id string) error {
	w.events = append(w.events, "stop:"+id)
	w.stopped = append(w.stopped, id)
	for i := range w.reps {
		if w.reps[i].ID == id {
			w.reps[i].Running = false
		}
	}
	return nil
}
func (w *world) Remove(_ context.Context, id string) error {
	w.events = append(w.events, "rm:"+id)
	for i := range w.reps {
		if w.reps[i].ID == id {
			w.reps = append(w.reps[:i], w.reps[i+1:]...)
			break
		}
	}
	return nil
}
func (w *world) Exec(context.Context, string, []string, ...string) (string, error) { return "", nil }
func (w *world) Image(context.Context, string) (string, []string, error)           { return "", nil, nil }
func (w *world) Wait(context.Context, string, time.Duration)                       { w.now = w.now.Add(5 * time.Second) }

// engine.Proxy: only listed hosts appear, healthy if running (new ones only when newHealthy).
func (w *world) SetList(_ context.Context, names []string) error {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	w.events = append(w.events, "list:"+strings.Join(sorted, ","))
	w.list = map[string]bool{}
	for _, n := range names {
		w.list[n] = true
	}
	w.cds = envoy.Clusters(w.svc, names)
	return nil
}
func (w *world) Current(context.Context) (string, error) { return w.cds, nil }
func (w *world) Health(context.Context) (map[string]bool, error) {
	sick := w.sick > 0
	if sick {
		w.sick--
	}
	out := map[string]bool{}
	for _, r := range w.reps {
		if !w.list[r.Name] {
			continue
		}
		isNew := r.Labels[revision.LabelSpecHash] == w.desired
		out[r.Name] = !sick && r.Running && (!isNew || w.newHealthy) && !w.never[r.Name]
	}
	return out, nil
}
func (w *world) Conns(_ context.Context, ips []string) (int, error) {
	w.connPolls++
	if w.connErrs > 0 {
		w.connErrs--
		return 0, fmt.Errorf("exec failed")
	}
	for _, r := range w.reps {
		if len(r.IPs) > 0 && r.IPs[0] == ips[0] && w.conns[r.Name] > 0 {
			w.conns[r.Name]--
			return w.conns[r.Name] + 1, nil
		}
	}
	return 0, nil
}
func (w *world) HTTP(_ context.Context, method, host string, _ int, path string) (int, error) {
	w.http = append(w.http, method+" "+host+" "+path)
	if path == "/safe" {
		if w.onSafe != nil {
			w.onSafe()
		}
		if w.unsafe > 0 {
			w.unsafe--
			return 503, nil
		}
	}
	return 200, nil
}

// Scaler
func (w *world) ScaleUp(_ context.Context, app types.ServiceConfig, total int) error {
	w.scaled++
	for len(w.reps) < total {
		w.add(app.Labels[revision.LabelSpecHash])
	}
	return nil
}
