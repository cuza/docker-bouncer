package cli

import (
	"testing"
	"time"

	"github.com/cuza/docker-bouncer/internal/revision"
)

func TestUndoTarget(t *testing.T) {
	h := []revision.Entry{{Revision: 7, UpID: "u7"}, {Revision: 6, UpID: "u6"}, {Revision: 5, UpID: "u5"}}
	if e, err := undoTarget(h, 0); err != nil || e.Revision != 6 {
		t.Fatalf("default is the previous revision: %+v %v", e, err)
	}
	if e, err := undoTarget(h, 5); err != nil || e.Revision != 5 {
		t.Fatalf("explicit: %+v %v", e, err)
	}
	if _, err := undoTarget(h, 7); err == nil {
		t.Fatal("undo to the running revision is an error")
	}
	if _, err := undoTarget(h[:1], 0); err == nil {
		t.Fatal("nothing to undo")
	}
	_ = time.Now
}

func TestServicesOfLastUp(t *testing.T) {
	latest := map[string]revision.Entry{
		"api":    {Revision: 4, UpID: "20261003T120000Z"},
		"worker": {Revision: 9, UpID: "20261003T120000Z"},
		"admin":  {Revision: 2, UpID: "20261001T080000Z"},
	}
	got := servicesOfLastUp(latest)
	if len(got) != 2 || got[0] != "api" || got[1] != "worker" {
		t.Fatalf("got %v", got)
	}
}
