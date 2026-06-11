//go:build integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// TestRealtimeEndToEnd exercises the whole realtime path against the
// real database: websocket subscribe -> store write -> pg_notify ->
// listener -> hub -> websocket event.
func TestRealtimeEndToEnd(t *testing.T) {
	store := newTestStore(t)
	hub := NewHub()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listener := &Listener{dsn: testDSN(), store: store, hub: hub}
	go listener.Run(ctx)

	srv := &Server{
		store:       store,
		hub:         hub,
		policies:    NewPolicyStore(t.TempDir(), 0),
		quickDomain: "quick.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Forwarded-Host": []string{"it-rt.quick.localhost"}},
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.CloseNow()

	if err := wsjson.Write(ctx, conn, wsRequest{Type: "subscribe", Collection: "rt-posts"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Both the LISTEN setup and the subscribe are asynchronous, so
	// create repeatedly until the first event arrives.
	var created Document
	var got Event
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no realtime event arrived within 15s")
		}
		doc, err := store.Create(ctx, "it-rt", "rt-posts", map[string]any{"n": float64(1)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		created = doc

		// Fresh struct per read: unmarshal does not reset fields the
		// JSON omits, so reuse would leak data across events.
		var ev Event
		readCtx, readCancel := context.WithTimeout(ctx, time.Second)
		err = wsjson.Read(readCtx, conn, &ev)
		readCancel()
		if err == nil {
			got = ev
			break
		}
	}
	if got.Type != "create" || got.Collection != "rt-posts" {
		t.Fatalf("event = %+v, want create on rt-posts", got)
	}
	if got.Doc == nil || got.Doc.Data["n"] != float64(1) {
		t.Errorf("event doc = %+v, want the created document body", got.Doc)
	}

	// Once the pipeline is warm, a delete event must arrive promptly
	// and carry no body.
	if err := store.Delete(ctx, "it-rt", "rt-posts", created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for {
		var ev Event
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		err := wsjson.Read(readCtx, conn, &ev)
		readCancel()
		if err != nil {
			t.Fatalf("waiting for delete event: %v", err)
		}
		// Skip create events from the warm-up retries.
		if ev.Type == "delete" && ev.ID == created.ID {
			got = ev
			break
		}
	}
	if got.Doc != nil {
		t.Errorf("delete event carries a doc: %+v", got.Doc)
	}
}
