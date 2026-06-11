package main

import "testing"

func TestHubRouting(t *testing.T) {
	hub := NewHub()
	a := make(chan Event, 2)
	b := make(chan Event, 2)
	hub.Subscribe("site1", "posts", a)
	hub.Subscribe("site2", "posts", b)

	hub.Publish("site1", "posts", Event{Type: "create", ID: "x"})
	if len(a) != 1 {
		t.Fatalf("subscriber a got %d events, want 1", len(a))
	}
	if ev := <-a; ev.Type != "create" || ev.ID != "x" {
		t.Errorf("subscriber a got %+v", ev)
	}
	if len(b) != 0 {
		t.Errorf("subscriber b (other scope) got %d events, want 0", len(b))
	}

	// Same scope+collection reaches all subscribers — the shared-* case.
	c := make(chan Event, 2)
	hub.Subscribe("_shared", "shared-libs", a)
	hub.Subscribe("_shared", "shared-libs", c)
	hub.Publish("_shared", "shared-libs", Event{Type: "create", ID: "y"})
	if len(a) != 1 || len(c) != 1 {
		t.Errorf("shared publish reached a=%d c=%d subscribers, want 1 each", len(a), len(c))
	}
}

func TestHubUnsubscribeAndFullBuffers(t *testing.T) {
	hub := NewHub()
	a := make(chan Event, 1)
	hub.Subscribe("site1", "posts", a)
	hub.UnsubscribeAll(a)
	hub.Publish("site1", "posts", Event{Type: "create"})
	if len(a) != 0 {
		t.Errorf("unsubscribed channel got %d events, want 0", len(a))
	}

	// A full buffer drops events instead of blocking the fan-out.
	full := make(chan Event, 1)
	hub.Subscribe("site1", "posts", full)
	hub.Publish("site1", "posts", Event{Type: "create", ID: "1"})
	hub.Publish("site1", "posts", Event{Type: "create", ID: "2"})
	if len(full) != 1 {
		t.Errorf("full channel holds %d events, want 1 (second dropped)", len(full))
	}
}
