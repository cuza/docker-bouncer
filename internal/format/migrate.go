package format

import (
	"encoding/json"
	"fmt"
)

// Payloads are what a replica stores in its labels, decoded: the spec of
// every revision (the current one, then the history's) and the invocation.
type Payloads struct {
	Specs      []json.RawMessage
	Invocation json.RawMessage
}

// Step migrates Payloads from one format to the next; Fn is pure.
type Step struct {
	From, To Format
	Fn       func(Payloads) (Payloads, error)
}

// Steps is the migration chain, oldest first, each From the previous To and
// the last To Current. A format bump needs a migration step and its test.
// Stacks from before formats were recorded read as 1.0: no step.
var Steps []Step

// Migrate runs p, stored at from, through the chain up to Current. A
// payload already at (or past) Current is returned as is.
func Migrate(steps []Step, from Format, p Payloads) (Payloads, error) {
	for f := from; f.Less(Current); {
		i := 0
		for i < len(steps) && steps[i].From != f {
			i++
		}
		if i == len(steps) {
			return p, fmt.Errorf("no migration from format %s", f)
		}
		var err error
		if p, err = steps[i].Fn(p); err != nil {
			return p, fmt.Errorf("migrating format %s to %s: %w", f, steps[i].To, err)
		}
		f = steps[i].To
	}
	return p, nil
}

// Chain checks steps form one ordered chain ending at Current.
func Chain(steps []Step) error {
	for i, s := range steps {
		if !s.From.Less(s.To) {
			return fmt.Errorf("step %d: %s → %s goes backwards", i, s.From, s.To)
		}
		if i > 0 && steps[i-1].To != s.From {
			return fmt.Errorf("step %d starts at %s, the previous ends at %s", i, s.From, steps[i-1].To)
		}
	}
	if n := len(steps); n > 0 && steps[n-1].To != Current {
		return fmt.Errorf("the chain ends at %s, not %s", steps[n-1].To, Current)
	}
	return nil
}
