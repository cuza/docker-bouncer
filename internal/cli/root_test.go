package cli

import "testing"

// Merging persistent flags panics on a shorthand clash (e.g. -f).
func TestNoFlagClashes(t *testing.T) {
	root := NewRoot(nil)
	for _, c := range root.Commands() {
		if _, _, err := root.Find([]string{c.Name(), "x"}); err != nil {
			t.Fatal(err)
		}
	}
}
