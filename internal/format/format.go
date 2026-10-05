// Package format versions how Bouncer stores its state on containers (the
// dev.cuza.bouncer.* labels, the spec, history and invocation encodings), so
// a CLI can tell a stack it may change from one it would misread.
//
// The format is MAJOR.MINOR, written on every replica and lock:
//   - bump Minor for an additive change older CLIs can safely ignore (a new
//     optional label or bundle field);
//   - bump Major only when older CLIs would misread or corrupt state;
//   - raise MinReadableMajor only when a release drops reading an old major.
//
// The bouncer version label beside it is informational only: compatibility
// is never decided by comparing versions.
package format

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	Major            = 1
	Minor            = 0
	MinReadableMajor = 1
)

// Current is the format this CLI writes.
var Current = Format{Major, Minor}

type Format struct{ Major, Minor int }

func (f Format) String() string { return fmt.Sprintf("%d.%d", f.Major, f.Minor) }

func (f Format) Less(o Format) bool {
	return f.Major < o.Major || f.Major == o.Major && f.Minor < o.Minor
}

// Parse reads "MAJOR.MINOR".
func Parse(s string) (Format, error) {
	maj, min, ok := strings.Cut(s, ".")
	a, err1 := strconv.Atoi(maj)
	b, err2 := strconv.Atoi(min)
	if !ok || err1 != nil || err2 != nil || a < 0 || b < 0 {
		return Format{}, fmt.Errorf("format %q is not MAJOR.MINOR", s)
	}
	return Format{a, b}, nil
}
