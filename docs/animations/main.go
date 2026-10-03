// Command animations renders docs/bounce-<method>.svg by running the real
// bounce planner (bounce.Plan) against a small simulated world: three v1
// replicas replaced by three v2 ones. Run it from the repository root:
//
//	go run ./docs/animations
package main

import (
	"fmt"
	"html"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/cuza/docker-bouncer/internal/bounce"
	"github.com/cuza/docker-bouncer/internal/config"
	"github.com/cuza/docker-bouncer/internal/engine"
)

const n = 3

var names = []string{"app-1", "app-2", "app-3", "app-4", "app-5", "app-6"}

type method struct {
	name     string
	settings map[string]any // x-bouncer keys besides bounce_method and min_task_uptime
	label    string
	summary  string
}

var methods = []method{
	{config.MethodCrossover, map[string]any{"bounce_overprovision_factor": 0.33},
		"bounce_method: crossover · bounce_overprovision_factor: 0.33 · replicas: 3",
		"crossover: a new replica joins before an old one leaves; capacity never drops"},
	{config.MethodUpThenDown, nil,
		"bounce_method: upthendown · defaults · replicas: 3",
		"upthendown: every new replica is healthy before any old one drains"},
	{config.MethodDownThenUp, nil,
		"bounce_method: downthenup · defaults · replicas: 3",
		"downthenup: every old replica goes first; requests fail until a new one is healthy"},
	{config.MethodBrutal, nil,
		"bounce_method: brutal · defaults · replicas: 3",
		"brutal: no health gate, no drain wait (development only)"},
}

// frame is one visible state: each replica's state ("" = not created yet,
// starting, serving, draining, gone), Envoy's list and a caption.
type frame struct {
	State   map[string]string
	List    []string
	Caption string
}

func (f frame) count(state string) int {
	c := 0
	for _, s := range f.State {
		if s == state {
			c++
		}
	}
	return c
}

type replica struct {
	name             string
	hash             string
	start, healthyAt int // ticks
	draining, gone   bool
}

type world struct {
	tick int
	reps []*replica
	list []string
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(tick int) time.Time { return t0.Add(time.Duration(tick) * time.Second) }

func (w *world) observe(spec config.Spec) bounce.State {
	st := bounce.State{N: n, Spec: spec, Desired: "v2", Now: at(w.tick), RunStart: t0}
	for _, r := range w.reps {
		if r.gone {
			continue
		}
		o := bounce.Observed{
			Replica: engine.Replica{Name: r.name, Running: true, Created: at(r.start)},
			Hash:    r.hash,
			Listed:  slices.Contains(w.list, r.name),
		}
		if w.tick >= r.healthyAt {
			o.EnvoyHealthy, o.HealthySince = true, at(r.healthyAt)
		}
		st.Replicas = append(st.Replicas, o)
	}
	return st
}

func (w *world) snapshot() frame {
	f := frame{State: map[string]string{}, List: slices.Clone(w.list)}
	for _, r := range w.reps {
		s := "starting"
		switch {
		case r.gone:
			s = "gone"
		case r.draining:
			s = "draining"
		case w.tick >= r.healthyAt && slices.Contains(w.list, r.name):
			s = "serving"
		}
		f.State[r.name] = s
	}
	return f
}

func (w *world) find(name string) *replica {
	for _, r := range w.reps {
		if r.name == name {
			return r
		}
	}
	panic("no replica " + name)
}

func spec(m method) config.Spec {
	x := map[string]any{"bounce_method": m.name, "min_task_uptime": "1s"}
	for k, v := range m.settings {
		x[k] = v
	}
	sc := types.ServiceConfig{Name: "api", Extensions: types.Extensions{config.Extension: x}}
	sc.Expose = types.StringOrNumberList{"8080"} // promoted field: not allowed in the literal
	svc, err := config.Parse(sc)
	if err != nil {
		log.Fatal(err)
	}
	return svc.Spec
}

// simulate runs bounce.Plan until Done and returns the visible frames.
// A new replica turns Envoy-healthy 2 ticks after it starts (+1 per later
// one); a drain takes 1 tick (brutal: none). After Done the world keeps
// ticking until every replica is healthy (brutal finishes before that).
func simulate(m method) []frame {
	sp := spec(m)
	w := &world{list: []string{"app-1", "app-2", "app-3"}}
	for _, name := range names[:n] {
		w.reps = append(w.reps, &replica{name: name, hash: "v1", start: -10, healthyAt: -10})
	}
	frames := []frame{w.snapshot()}
	record := func() {
		f := w.snapshot()
		last := frames[len(frames)-1]
		if !maps(f.State, last.State) || !slices.Equal(f.List, last.List) {
			frames = append(frames, f)
		}
	}
	created := n
	for i := 0; ; i++ {
		if i > 200 {
			panic(m.name + ": planner did not finish")
		}
		step := bounce.Plan(w.observe(sp))
		if step.Kind == bounce.Done {
			break
		}
		switch step.Kind {
		case bounce.Wait:
			w.tick++
		case bounce.SetList:
			w.list = step.List
		case bounce.ScaleUp:
			for alive := len(w.observe(sp).Replicas); alive < step.Total; alive++ {
				w.reps = append(w.reps, &replica{name: names[created], hash: "v2", start: w.tick, healthyAt: w.tick + 2 + created - n})
				created++
			}
		case bounce.Drain:
			r := w.find(step.Replica.Name)
			w.list = slices.DeleteFunc(slices.Clone(w.list), func(s string) bool { return s == r.name })
			if sp.BounceMethod != config.MethodBrutal {
				r.draining = true
				record()
				w.tick++
			}
			r.draining, r.gone = false, true
		default:
			panic(fmt.Sprintf("%s: unexpected step %+v", m.name, step))
		}
		record()
	}
	for w.snapshot().count("serving") < n {
		w.tick++
		record()
	}
	captions(frames)
	final := frames[len(frames)-1]
	final.Caption = "v2 serves all traffic. Bounce done."
	return append(frames, final)
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// captions describes each frame by what changed since the previous one.
func captions(frames []frame) {
	frames[0].Caption = "v1 serves all traffic"
	for i := 1; i < len(frames); i++ {
		prev, cur := frames[i-1], frames[i]
		var parts []string
		for _, name := range names {
			p, c := prev.State[name], cur.State[name]
			v := "v1"
			if slices.Index(names, name) >= n {
				v = "v2"
			}
			listed := slices.Contains(cur.List, name) && !slices.Contains(prev.List, name)
			switch {
			case p == "" && c != "":
				parts = append(parts, name+" ("+v+") starts")
			case listed && c == "starting":
				parts = append(parts, name+" added to Envoy's list (no traffic until healthy)")
			case listed:
				parts = append(parts, name+" added to Envoy's list")
			case p != "serving" && c == "serving":
				parts = append(parts, name+" healthy → Envoy sends it traffic")
			case c == "draining" && p != "draining":
				parts = append(parts, name+" leaves the list; in-flight requests finish")
			case c == "gone" && p == "draining":
				parts = append(parts, name+" stopped and removed")
			case c == "gone" && p != "gone":
				parts = append(parts, name+" off the list, stopped and removed at once")
			}
		}
		cur.Caption = strings.Join(parts, "; ")
		if cur.count("serving") == 0 {
			cur.Caption += " · nothing serving, requests fail"
		}
		frames[i] = cur
	}
}

// layout
const (
	width, height = 760, 390
	stepSec       = 3.75
	proxyY        = 198
	v1X, v2X      = 462, 614
	boxW, boxH    = 130, 50
)

func rowY(i int) int { // v2 rows sit in the gaps between v1 rows
	if i < n {
		return 112 + 86*i
	}
	return 155 + 86*(i-n)
}

func boxX(i int) int {
	if i < n {
		return v1X
	}
	return v2X
}

func wire(i int) string {
	y := rowY(i)
	if i < n {
		return fmt.Sprintf("M400 %d C431 %d 431 %d %d %d", proxyY, proxyY, y, v1X, y)
	}
	return fmt.Sprintf("M400 %d C425 %d 425 %d 450 %d H%d", proxyY, proxyY, y, y, v2X)
}

func route(i int) string {
	y := rowY(i)
	if i < n {
		return fmt.Sprintf("0%% { transform: translate(134px, %dpx) } 15%% { transform: translate(190px, %dpx) } 70%% { transform: translate(400px, %dpx) } 100%% { transform: translate(%dpx, %dpx) }",
			proxyY, proxyY, proxyY, v1X, y)
	}
	return fmt.Sprintf("0%% { transform: translate(134px, %dpx) } 12%% { transform: translate(190px, %dpx) } 55%% { transform: translate(400px, %dpx) } 65%% { transform: translate(450px, %dpx) } 100%% { transform: translate(%dpx, %dpx) }",
		proxyY, proxyY, proxyY, y, v2X, y)
}

const style = `    :root {
      --bg: #ffffff; --fg: #1f2328; --muted: #59636e; --box: #f6f8fa; --line: #d1d9e0;
      --ok: #1a7f37; --warn: #9a6700; --idle: #818b98; --dot: #0969da; --bad: #cf222e;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #0d1117; --fg: #e6edf3; --muted: #9198a1; --box: #151b23; --line: #3d444d;
        --ok: #3fb950; --warn: #d29922; --idle: #6e7681; --dot: #4493f8; --bad: #f85149;
      }
    }
    text { font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; fill: var(--fg); }
    .bg { fill: var(--bg); }
    .box { fill: var(--box); stroke: var(--line); stroke-width: 1.5; }
    .title { font-size: 15px; font-weight: 600; }
    .sub { font-size: 12px; fill: var(--muted); }
    .wire { stroke: var(--line); stroke-width: 2; fill: none; }
    .cap { font-size: 14px; font-weight: 500; }
    .ok-rect { fill: none; stroke: var(--ok); stroke-width: 2.5; }
    .warn-rect { fill: none; stroke: var(--warn); stroke-width: 2.5; stroke-dasharray: 6 5; }
    .idle-rect { fill: none; stroke: var(--idle); stroke-width: 2.5; stroke-dasharray: 2 4; }
    .ok { fill: var(--ok); } .warn { fill: var(--warn); } .idle { fill: var(--idle); }
    .dot { fill: var(--dot); } .bad { fill: var(--bad); }
    .t { animation: 2.4s linear infinite; }
    .d2 { animation-delay: -0.8s } .d3 { animation-delay: -1.6s }
    @keyframes fail {
      0% { transform: translate(134px, 198px); opacity: 1 }
      40% { transform: translate(190px, 198px); opacity: 1 }
      60% { transform: translate(240px, 198px); opacity: 0 }
      100% { transform: translate(240px, 198px); opacity: 0 }
    }
    .fail { animation-name: fail }
    #static { opacity: 0 }
`

// render draws the frames; every element that changes gets keyframes built
// from its per-frame visibility, and its opacity attribute is its state in
// the last frame (what prefers-reduced-motion shows).
func render(m method, frames []frame) string {
	var css, body strings.Builder
	kf := map[string]bool{}
	anim := func(id string, vis func(int) bool) string {
		var key strings.Builder
		for i := range frames {
			key.WriteByte("01"[b2i(vis(i))])
		}
		name := "k" + key.String()
		if !kf[name] {
			kf[name] = true
			fmt.Fprintf(&css, "    @keyframes %s {", name)
			for i := range frames {
				s, e := float64(i)*100/float64(len(frames)), float64(i+1)*100/float64(len(frames))
				if i > 0 {
					s += 0.4
				}
				fmt.Fprintf(&css, " %.2f%%,%.2f%% { opacity: %d }", s, e, b2i(vis(i)))
			}
			css.WriteString(" }\n")
		}
		fmt.Fprintf(&css, "    #%s { animation-name: %s }\n", id, name)
		return fmt.Sprintf(`id="%s" opacity="%d"`, id, b2i(vis(len(frames)-1)))
	}
	w := func(format string, a ...any) { fmt.Fprintf(&body, format+"\n", a...) }

	w(`  <rect class="bg" x="0" y="0" width="%d" height="%d" rx="12"/>`, width, height)
	w(`  <text class="sub" x="24" y="26">%s</text>`, esc(m.label))
	w(`  <path class="wire" d="M134 %d H190"/>`, proxyY)
	w(`  <rect class="box" x="24" y="170" width="110" height="56" rx="10"/>`)
	w(`  <text class="title" x="79" y="195" text-anchor="middle">clients</text>`)
	w(`  <text class="sub" x="79" y="213" text-anchor="middle">api:8080</text>`)
	w(`  <rect class="box" x="190" y="150" width="210" height="96" rx="10"/>`)
	w(`  <text class="title" x="295" y="176" text-anchor="middle">api</text>`)
	w(`  <text class="sub" x="295" y="194" text-anchor="middle">Envoy proxy</text>`)
	var lists []string
	for _, f := range frames {
		if l := strings.Join(f.List, ", "); !slices.Contains(lists, l) {
			lists = append(lists, l)
		}
	}
	for i, l := range lists {
		text := "list: " + l
		if l == "" {
			text = "list: (empty)"
		}
		if len(text) > 30 { // wrap after the third name to stay inside the proxy box
			cut := strings.LastIndex(text[:30], ", ")
			text = esc(text[:cut+1]) + `<tspan x="295" dy="16">` + esc(text[cut+2:]) + `</tspan>`
		} else {
			text = esc(text)
		}
		w(`  <text %s class="sub a" x="295" y="218" text-anchor="middle">%s</text>`,
			anim(fmt.Sprintf("list%d", i), func(i int) bool { return strings.Join(frames[i].List, ", ") == l }), text)
	}

	is := func(name string, states ...string) func(int) bool {
		return func(i int) bool { return slices.Contains(states, frames[i].State[name]) }
	}
	for i, name := range names {
		x, y := boxX(i), rowY(i)
		v := "v1"
		if i >= n {
			v = "v2"
		}
		w(`  <g %s class="a">`, anim(name, is(name, "starting", "serving", "draining")))
		w(`    <path class="wire" d="%s"/>`, wire(i))
		w(`    <rect class="box" x="%d" y="%d" width="%d" height="%d" rx="10"/>`, x, y-boxH/2, boxW, boxH)
		for _, s := range []struct{ state, rect, cls, text string }{
			{"starting", "idle-rect", "idle", "starting"},
			{"serving", "ok-rect", "ok", "healthy · serving"},
			{"draining", "warn-rect", "warn", "draining"},
		} {
			w(`    <rect %s class="%s a" x="%d" y="%d" width="%d" height="%d" rx="10"/>`,
				anim(name+"-"+s.state+"-r", is(name, s.state)), s.rect, x, y-boxH/2, boxW, boxH)
			w(`    <text %s class="sub a %s" x="%d" y="%d" text-anchor="middle">%s</text>`,
				anim(name+"-"+s.state+"-t", is(name, s.state)), s.cls, x+boxW/2, y+15, esc(s.text))
		}
		w(`    <text class="title" x="%d" y="%d" text-anchor="middle">%s · %s</text>`, x+boxW/2, y-3, name, v)
		w(`    <circle %s class="dot a" cx="%d" cy="%d" r="5"/>`, anim(name+"-inflight", is(name, "draining")), x+14, y-3)
		w(`  </g>`)
	}

	// request dots: only to listed, healthy replicas; nowhere when none is
	for i, name := range names {
		fmt.Fprintf(&css, "    @keyframes to%d { %s }\n    .to%d { animation-name: to%d }\n", i, route(i), i, i)
		w(`  <g %s class="a route">`, anim(name+"-route", is(name, "serving")))
		for _, d := range []string{"", " d2", " d3"} {
			w(`    <circle class="dot t to%d%s" r="5"/>`, i, d)
		}
		w(`  </g>`)
	}
	w(`  <g %s class="a route">`, anim("failing", func(i int) bool { return frames[i].count("serving") == 0 }))
	for _, d := range []string{"", " d2", " d3"} {
		w(`    <circle class="bad t fail%s" r="5"/>`, d)
	}
	w(`  </g>`)

	for i, f := range frames {
		w(`  <text %s class="cap a" x="24" y="52">%d · %s</text>`, anim(fmt.Sprintf("c%d", i), func(j int) bool { return j == i }), i+1, esc(f.Caption))
	}
	w(`  <text id="static" class="cap" x="24" y="52">%s</text>`, esc(m.summary))
	w(`  <text class="sub" x="24" y="378">clients never reconnect: they talk to the proxy, only the proxy's upstream list changes</text>`)

	total := float64(len(frames)) * stepSec
	var caps []string
	for i, f := range frames {
		caps = append(caps, fmt.Sprintf("%d. %s.", i+1, strings.TrimSuffix(f.Caption, ".")))
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-labelledby="t d">
  <title id="t">docker bouncer: a %s bounce</title>
  <desc id="d">%s %s</desc>
  <style>
%s    /* %d frames × %.2f s = %.2f s loop; generated by go run ./docs/animations */
    .a { animation-duration: %.2fs; animation-iteration-count: infinite; animation-timing-function: linear; }
%s    @media (prefers-reduced-motion: reduce) {
      .a, .t { animation: none !important; }
      .route, .cap { opacity: 0 !important; }
      #static { opacity: 1 !important; }
    }
  </style>

%s</svg>
`, width, height, width, height, m.name, esc(m.label), esc(strings.Join(caps, " ")),
		style, len(frames), stepSec, total, total, css.String(), body.String())
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func esc(s string) string { return html.EscapeString(s) }

func main() {
	for _, m := range methods {
		path := filepath.Join("docs", "bounce-"+m.name+".svg")
		if err := os.WriteFile(path, []byte(render(m, simulate(m))), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println(path)
	}
}
