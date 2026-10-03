package lock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type fake struct {
	n            int
	ids          map[string]string // name -> id
	created      map[string]time.Time
	labels       map[string]map[string]string
	beforeRemove func() // hook to simulate a racing acquirer
}

func newFake() *fake {
	return &fake{ids: map[string]string{}, created: map[string]time.Time{}, labels: map[string]map[string]string{}}
}

func (f *fake) Create(_ context.Context, name, _ string, labels map[string]string) (string, error) {
	if _, ok := f.ids[name]; ok {
		return "", ErrConflict
	}
	f.n++
	f.ids[name], f.created[name], f.labels[name] = fmt.Sprint(f.n), now, labels
	return f.ids[name], nil
}
func (f *fake) Inspect(_ context.Context, name string) (string, time.Time, map[string]string, error) {
	return f.ids[name], f.created[name], f.labels[name], nil
}
func (f *fake) Remove(_ context.Context, id string) error {
	if h := f.beforeRemove; h != nil {
		f.beforeRemove = nil
		h()
	}
	for name, i := range f.ids {
		if i == id {
			delete(f.ids, name)
		}
	}
	return nil
}

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func TestAcquireAndRelease(t *testing.T) {
	f := newFake()
	rel, err := Acquire(context.Background(), f, "proj", "img", "alice@host", time.Hour, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.ids["proj-bouncer-lock"]; !ok {
		t.Fatal("lock container not created")
	}
	_, err = Acquire(context.Background(), f, "proj", "img", "bob@host", time.Hour, false, now)
	var held ErrHeld
	if !errors.As(err, &held) || held.Owner != "alice@host" {
		t.Fatalf("got %v", err)
	}
	if err := rel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.ids["proj-bouncer-lock"]; ok {
		t.Fatal("release must remove the lock")
	}
}

func TestStaleAndForcedLocksAreTakenOver(t *testing.T) {
	f := newFake()
	relAlice, _ := Acquire(context.Background(), f, "proj", "img", "alice", time.Hour, false, now)
	if _, err := Acquire(context.Background(), f, "proj", "img", "bob", time.Hour, false, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if _, err := Acquire(context.Background(), f, "proj", "img", "carol", time.Hour, true, now); err != nil {
		t.Fatalf("forced: %v", err)
	}
	relAlice(context.Background()) // a late release must not drop carol's lock
	if f.labels["proj-bouncer-lock"][LabelLockOwner] != "carol" || f.ids["proj-bouncer-lock"] == "" {
		t.Fatal("stale holder's release removed the new holder's lock")
	}
}

// Two takers see the same stale lock; the one that loses the race must not
// remove the winner's fresh lock.
func TestRacingTakeoverLeavesOneHolder(t *testing.T) {
	f := newFake()
	Acquire(context.Background(), f, "proj", "img", "alice", time.Hour, false, now)
	later := now.Add(2 * time.Hour)
	f.beforeRemove = func() { // bob's takeover completes while carol is about to remove the stale lock
		for name := range f.ids {
			delete(f.ids, name)
		}
		f.n++
		f.ids["proj-bouncer-lock"], f.created["proj-bouncer-lock"] = fmt.Sprint(f.n), later
		f.labels["proj-bouncer-lock"] = map[string]string{LabelLockOwner: "bob"}
	}
	_, err := Acquire(context.Background(), f, "proj", "img", "carol", time.Hour, false, later)
	var held ErrHeld
	if !errors.As(err, &held) || held.Owner != "bob" {
		t.Fatalf("got %v", err)
	}
}
