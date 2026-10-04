package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 5e6, time.FixedZone("CEST", 2*3600))

func TestPlainEvents(t *testing.T) {
	ev := api.Resource{ID: "Container proj-web-app-1", Status: api.Done, Text: "Stopped", Details: "drained 2s"}
	for _, tc := range []struct {
		stamp bool
		want  string
	}{
		{false, " Container proj-web-app-1 Stopped drained 2s\n"},
		{true, "2026-10-03T10:00:00.005Z Container proj-web-app-1 Stopped drained 2s\n"},
	} {
		var b bytes.Buffer
		w := display.Plain(&b)
		if tc.stamp {
			w = display.Plain(stamped{&b, func() time.Time { return t0 }})
		}
		w.On(ev)
		if b.String() != tc.want {
			t.Errorf("timestamps=%v: %q, want %q", tc.stamp, b.String(), tc.want)
		}
	}
}

func TestQuietKeepsErrors(t *testing.T) {
	var b bytes.Buffer
	errorsOnly{display.Plain(&b)}.On(
		api.Resource{ID: "Service web", Status: api.Working, Text: "Bouncing (0/2)"},
		api.Resource{ID: "Service web", Status: api.Error, Text: "Failed: x"})
	if b.String() != " Service web Failed: x \n" {
		t.Fatalf("%q", b.String())
	}
}

func TestJSONEvents(t *testing.T) {
	var b bytes.Buffer
	j := &jsonEvents{out: &b, now: func() time.Time { return t0 }, project: "proj",
		services: map[string]string{"web": "web", "web-app": "web"}}
	j.On(
		api.Resource{ID: "Service web", Status: api.Done, Text: "Converged", Details: "2 replicas"},
		api.Resource{ID: "Container proj-web-app-1", Status: api.Working, Text: "Draining"},
		api.Resource{ID: "Container proj-web-1", Status: api.Done, Text: "Started"},
		api.Resource{ID: "Container proj-db-1", Status: api.Warning, Text: "x"},
		api.Resource{ID: "Image nginx", Status: api.Error, Text: "Error", Details: "boom"},
	)
	want := []jsonEvent{
		{Service: "web", Status: "Done", Message: "Converged 2 replicas"},
		{Service: "web", Replica: "proj-web-app-1", Status: "Working", Message: "Draining"},
		{Service: "web", Status: "Done", Message: "Started"},
		{Service: "db", Status: "Warning", Message: "x"},
		{Status: "Error", Message: "Error boom"},
	}
	dec := json.NewDecoder(&b)
	for i, w := range want {
		var got jsonEvent
		if err := dec.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Time != "2026-10-03T10:00:00.005Z" || got.Project != "proj" || got.Service != w.Service ||
			got.Replica != w.Replica || got.Status != w.Status || got.Message != w.Message {
			t.Errorf("event %d: %+v, want %+v", i, got, w)
		}
	}
}

// Every object has project, "" for refresh's summary across projects.
func TestJSONSummaryHasProject(t *testing.T) {
	var b bytes.Buffer
	j := &jsonEvents{out: &b, now: func() time.Time { return t0 }}
	j.On(api.Resource{ID: "Refresh", Status: api.Done, Text: "2 projects: 2 up to date"})
	if !bytes.Contains(b.Bytes(), []byte(`"project":""`)) {
		t.Fatalf("%s", b.String())
	}
}
