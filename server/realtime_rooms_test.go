package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestRoomHubPresenceAndMessages(t *testing.T) {
	hub := NewRoomHub()
	a := make(chan RoomEvent, 4)
	b := make(chan RoomEvent, 4)
	alice := RoomUser{ID: "a", Email: "alice@example.com", Name: "Alice"}
	bob := RoomUser{ID: "b", Email: "bob@example.com", Name: "Bob"}

	hub.Join("site1", "control", alice, a)
	hub.Join("site1", "control", bob, b)
	hub.Join("site2", "control", RoomUser{ID: "c", Email: "c@example.com"}, make(chan RoomEvent, 4))

	drainPresence := func(ch <-chan RoomEvent) RoomEvent {
		t.Helper()
		var last RoomEvent
		for {
			select {
			case ev := <-ch:
				if ev.Type == "room_presence" {
					last = ev
				}
			default:
				return last
			}
		}
	}

	if ev := drainPresence(a); len(ev.Users) != 2 {
		t.Fatalf("site1 presence users = %+v, want alice and bob", ev.Users)
	} else if ev.SentAt != nil {
		t.Fatalf("site1 presence sent_at = %v, want omitted", ev.SentAt)
	}
	drainPresence(b)
	if ok := hub.Publish("site1", "control", "a", "cursor", json.RawMessage(`{"x":1}`)); !ok {
		t.Fatal("Publish returned false")
	}
	if len(a) != 0 {
		t.Fatalf("sender got echoed messages, want none")
	}
	got := <-b
	if got.Type != "room_message" || got.Event != "cursor" || got.From.Email != "alice@example.com" {
		t.Fatalf("room message = %+v", got)
	}
	if got.SentAt == nil {
		t.Fatal("room message sent_at is nil, want timestamp")
	}

	hub.SetPresence("site1", "control", "b", json.RawMessage(`{"role":"operator"}`))
	if ev := drainPresence(a); len(ev.Users) != 2 || roomUserByEmail(ev.Users, "bob@example.com").Data == nil {
		t.Fatalf("presence update = %+v, want bob data", ev.Users)
	}
	if ok := hub.Publish("site1", "control", "b", "cursor", json.RawMessage(`{"x":2}`)); !ok {
		t.Fatal("Publish after presence returned false")
	}
	got = <-a
	if got.From.Data != nil {
		t.Fatalf("room message sender data = %s, want omitted presence data", got.From.Data)
	}

	hub.Leave("site1", "control", "b")
	if ev := drainPresence(a); len(ev.Users) != 1 || ev.Users[0].Email != "alice@example.com" {
		t.Fatalf("presence after leave = %+v, want only alice", ev.Users)
	}
}

func TestWebSocketRooms(t *testing.T) {
	srv := &Server{
		resolver:   NewStaticResolver("operator@example.com", "Operator", nil),
		policies:   NewPolicyStore(t.TempDir(), 0),
		spotDomain: "spot.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	headers := http.Header{"X-Forwarded-Host": []string{"ops.spot.localhost"}}
	a, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial a: %v", err)
	}
	defer a.CloseNow()
	b, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial b: %v", err)
	}
	defer b.CloseNow()

	if err := wsjson.Write(ctx, a, wsRequest{Type: "room_join", Room: "control"}); err != nil {
		t.Fatalf("join a: %v", err)
	}
	if err := wsjson.Write(ctx, b, wsRequest{Type: "room_join", Room: "control"}); err != nil {
		t.Fatalf("join b: %v", err)
	}

	waitPresence := func(conn *websocket.Conn, want int) RoomEvent {
		t.Helper()
		for {
			var ev RoomEvent
			if err := wsjson.Read(ctx, conn, &ev); err != nil {
				t.Fatalf("read presence: %v", err)
			}
			if ev.Type == "room_presence" && len(ev.Users) == want {
				return ev
			}
		}
	}
	if ev := waitPresence(a, 2); ev.Room != "control" {
		t.Fatalf("presence room = %q, want control", ev.Room)
	}

	if err := wsjson.Write(ctx, a, wsRequest{
		Type:  "room_send",
		Room:  "control",
		Event: "cursor",
		Data:  json.RawMessage(`{"x":12,"y":8}`),
	}); err != nil {
		t.Fatalf("send cursor: %v", err)
	}

	got := waitRoomMessage(ctx, t, b)
	if got.Type != "room_message" || got.Event != "cursor" || got.From.Email != "operator@example.com" {
		t.Fatalf("cursor event = %+v", got)
	}
	if string(got.Data) != `{"x":12,"y":8}` {
		t.Fatalf("cursor data = %s", got.Data)
	}
}

func TestWebSocketRoomPresenceIsRateLimited(t *testing.T) {
	srv := &Server{
		resolver:      NewStaticResolver("operator@example.com", "Operator", nil),
		policies:      NewPolicyStore(t.TempDir(), 0),
		realtimeLimit: NewRateLimiter(0.001, 2),
		spotDomain:    "spot.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Forwarded-Host": []string{"ops.spot.localhost"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := wsjson.Write(ctx, conn, wsRequest{Type: "room_join", Room: "control"}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := wsjson.Write(ctx, conn, wsRequest{
		Type: "room_presence",
		Room: "control",
		Data: json.RawMessage(`{"n":1}`),
	}); err != nil {
		t.Fatalf("first presence: %v", err)
	}
	if err := wsjson.Write(ctx, conn, wsRequest{
		Type: "room_presence",
		Room: "control",
		Data: json.RawMessage(`{"n":2}`),
	}); err != nil {
		t.Fatalf("second presence: %v", err)
	}

	for {
		var ev struct {
			Type  string `json:"type"`
			Error string `json:"error"`
		}
		if err := wsjson.Read(ctx, conn, &ev); err != nil {
			t.Fatalf("read: %v", err)
		}
		if ev.Type == "error" {
			if ev.Error != "rate limit exceeded, slow down" {
				t.Fatalf("error = %q, want rate limit", ev.Error)
			}
			return
		}
	}
}

func roomUserByEmail(users []RoomUser, email string) RoomUser {
	for _, user := range users {
		if user.Email == email {
			return user
		}
	}
	return RoomUser{}
}

func waitRoomMessage(ctx context.Context, t *testing.T, conn *websocket.Conn) RoomEvent {
	t.Helper()
	for {
		var ev RoomEvent
		if err := wsjson.Read(ctx, conn, &ev); err != nil {
			t.Fatalf("read room message: %v", err)
		}
		if ev.Type == "room_message" {
			return ev
		}
	}
}
