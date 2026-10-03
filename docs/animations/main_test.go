package main

import (
	"slices"
	"testing"
)

// The animations come from bounce.Plan; these check each method's frames
// have the shape the method promises.
func TestFrames(t *testing.T) {
	old, cur := names[:n], names[n:]
	countIn := func(f frame, rs []string, states ...string) int {
		c := 0
		for _, r := range rs {
			if slices.Contains(states, f.State[r]) {
				c++
			}
		}
		return c
	}
	for _, m := range methods {
		frames := simulate(m)
		last := frames[len(frames)-1]
		if countIn(last, cur, "serving") != n || countIn(last, old, "gone") != n {
			t.Errorf("%s: last frame %v, want v2 serving and v1 gone", m.name, last.State)
		}
		gap, draining := false, false
		for i, f := range frames {
			serving, exist := f.count("serving"), countIn(f, names, "starting", "serving", "draining")
			gap = gap || serving == 0
			draining = draining || f.count("draining") > 0
			oldLeaving := countIn(f, old, "draining", "gone") > 0
			switch m.name {
			case "crossover":
				if serving < n || exist > n+1 {
					t.Errorf("crossover frame %d: %d serving, %d exist", i, serving, exist)
				}
			case "upthendown":
				if oldLeaving && countIn(f, cur, "serving") < n {
					t.Errorf("upthendown frame %d: old drained before all new healthy: %v", i, f.State)
				}
			case "brutal":
				if oldLeaving && countIn(f, cur, "starting", "serving") < n {
					t.Errorf("brutal frame %d: old removed before 3 new exist: %v", i, f.State)
				}
			}
		}
		if m.name == "downthenup" && !gap {
			t.Error("downthenup: no frame with 0 serving")
		}
		if m.name == "brutal" && draining {
			t.Error("brutal: has draining frames")
		}
	}
}
