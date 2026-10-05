package format

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStepsChain(t *testing.T) {
	if err := Chain(Steps); err != nil {
		t.Fatal(err)
	}
	bad := []Step{{From: Format{0, 1}, To: Format{0, 2}}, {From: Format{0, 3}, To: Current}}
	if err := Chain(bad); err == nil || !strings.Contains(err.Error(), "starts at 0.3") {
		t.Fatalf("a gap: %v", err)
	}
	if err := Chain([]Step{{From: Format{0, 2}, To: Format{0, 1}}}); err == nil {
		t.Fatal("a backwards step passed")
	}
}

// A fake two-step chain: 0.8 renames a spec field, 0.9 tags the invocation.
func TestMigrateRunsTheChain(t *testing.T) {
	steps := []Step{
		{From: Format{0, 8}, To: Format{0, 9}, Fn: func(p Payloads) (Payloads, error) {
			out := p
			out.Specs = nil
			for _, s := range p.Specs {
				out.Specs = append(out.Specs, json.RawMessage(strings.Replace(string(s), `"img"`, `"image"`, 1)))
			}
			return out, nil
		}},
		{From: Format{0, 9}, To: Current, Fn: func(p Payloads) (Payloads, error) {
			p.Invocation = json.RawMessage(strings.Replace(string(p.Invocation), "}", `,"migrated":true}`, 1))
			return p, nil
		}},
	}
	if err := Chain(steps); err != nil {
		t.Fatal(err)
	}
	in := Payloads{Specs: []json.RawMessage{[]byte(`{"img":"a"}`), []byte(`{"img":"b"}`)}, Invocation: []byte(`{"command":"up"}`)}
	got, err := Migrate(steps, Format{0, 8}, in)
	if err != nil || string(got.Specs[0]) != `{"image":"a"}` || string(got.Specs[1]) != `{"image":"b"}` || string(got.Invocation) != `{"command":"up","migrated":true}` {
		t.Fatalf("from 0.8: %s %s %s %v", got.Specs[0], got.Specs[1], got.Invocation, err)
	}
	if got, err := Migrate(steps, Format{0, 9}, in); err != nil || string(got.Specs[0]) != `{"img":"a"}` {
		t.Fatalf("from 0.9 only the last step runs: %s %v", got.Specs[0], err)
	}
	if got, err := Migrate(steps, Current, in); err != nil || string(got.Invocation) != `{"command":"up"}` {
		t.Fatalf("at Current nothing runs: %s %v", got.Invocation, err)
	}
	if _, err := Migrate(steps, Format{0, 5}, in); err == nil {
		t.Fatal("a format the chain doesn't cover migrated")
	}
}
