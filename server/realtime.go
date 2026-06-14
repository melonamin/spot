package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Event is what subscribed browsers receive over the websocket.
type Event struct {
	Type       string    `json:"type"`
	Collection string    `json:"collection"`
	ID         string    `json:"id"`
	Doc        *Document `json:"doc,omitempty"`
}

type subKey struct {
	scope      string
	collection string
}

// Hub fans document events out to websocket sessions. Each session
// registers one buffered channel; a slow consumer loses events rather
// than blocking the fan-out for everyone else.
type Hub struct {
	mu   sync.Mutex
	subs map[subKey]map[chan<- Event]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[subKey]map[chan<- Event]struct{}{}}
}

func (h *Hub) Subscribe(scope, collection string, out chan<- Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := subKey{scope, collection}
	if h.subs[key] == nil {
		h.subs[key] = map[chan<- Event]struct{}{}
	}
	h.subs[key][out] = struct{}{}
}

func (h *Hub) Unsubscribe(scope, collection string, out chan<- Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := subKey{scope, collection}
	delete(h.subs[key], out)
	if len(h.subs[key]) == 0 {
		delete(h.subs, key)
	}
}

func (h *Hub) UnsubscribeAll(out chan<- Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, set := range h.subs {
		delete(set, out)
		if len(set) == 0 {
			delete(h.subs, key)
		}
	}
}

func (h *Hub) Publish(scope, collection string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for out := range h.subs[subKey{scope, collection}] {
		select {
		case out <- ev:
		default:
		}
	}
}

type roomKey struct {
	scope string
	room  string
}

// RoomUser is the presence shape exposed to browsers for one websocket
// session in an ephemeral room.
type RoomUser struct {
	ID       string          `json:"id"`
	Email    string          `json:"email"`
	Name     string          `json:"name"`
	PeerName string          `json:"peer_name"`
	PeerIP   string          `json:"peer_ip"`
	Groups   []string        `json:"groups"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// RoomEvent is the internal websocket protocol for ephemeral realtime
// rooms. The browser SDK hides these message types behind room.send,
// room.on, and room.onPresence.
type RoomEvent struct {
	Type   string          `json:"type"`
	Room   string          `json:"room"`
	Event  string          `json:"event,omitempty"`
	From   *RoomUser       `json:"from,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Users  []RoomUser      `json:"users,omitempty"`
	SentAt *time.Time      `json:"sent_at,omitempty"`
}

type roomSub struct {
	user RoomUser
	out  chan<- RoomEvent
}

// RoomHub fans ephemeral room messages and presence snapshots out to
// websocket sessions. It is intentionally process-local; messages are
// transient and are not replayed to later subscribers.
type RoomHub struct {
	mu    sync.Mutex
	rooms map[roomKey]map[string]*roomSub
}

func NewRoomHub() *RoomHub {
	return &RoomHub{rooms: map[roomKey]map[string]*roomSub{}}
}

func (h *RoomHub) Join(scope, room string, user RoomUser, out chan<- RoomEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey{scope, room}
	if h.rooms[key] == nil {
		h.rooms[key] = map[string]*roomSub{}
	}
	h.rooms[key][user.ID] = &roomSub{user: user, out: out}
	h.publishPresenceLocked(key)
}

func (h *RoomHub) Leave(scope, room, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey{scope, room}
	if h.removeLocked(key, sessionID) {
		h.publishPresenceLocked(key)
	}
}

func (h *RoomHub) LeaveAll(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key := range h.rooms {
		if h.removeLocked(key, sessionID) {
			h.publishPresenceLocked(key)
		}
	}
}

func (h *RoomHub) SetPresence(scope, room, sessionID string, data json.RawMessage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey{scope, room}
	sub, ok := h.rooms[key][sessionID]
	if !ok {
		return false
	}
	sub.user.Data = data
	h.publishPresenceLocked(key)
	return true
}

func (h *RoomHub) Publish(scope, room, sessionID, event string, data json.RawMessage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey{scope, room}
	sender, ok := h.rooms[key][sessionID]
	if !ok {
		return false
	}
	from := sender.user
	from.Data = nil
	sentAt := time.Now().UTC()
	msg := RoomEvent{
		Type:   "room_message",
		Room:   room,
		Event:  event,
		From:   &from,
		Data:   data,
		SentAt: &sentAt,
	}
	for id, sub := range h.rooms[key] {
		if id == sessionID {
			continue
		}
		select {
		case sub.out <- msg:
		default:
		}
	}
	return true
}

func (h *RoomHub) removeLocked(key roomKey, sessionID string) bool {
	subs := h.rooms[key]
	if subs == nil {
		return false
	}
	if _, ok := subs[sessionID]; !ok {
		return false
	}
	delete(subs, sessionID)
	if len(subs) == 0 {
		delete(h.rooms, key)
	}
	return true
}

func (h *RoomHub) publishPresenceLocked(key roomKey) {
	subs := h.rooms[key]
	users := make([]RoomUser, 0, len(subs))
	for _, sub := range subs {
		users = append(users, sub.user)
	}
	ev := RoomEvent{Type: "room_presence", Room: key.room, Users: users}
	for _, sub := range subs {
		select {
		case sub.out <- ev:
		default:
		}
	}
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func roomUserFromIdentity(sessionID string, id Identity) RoomUser {
	return RoomUser{
		ID:       sessionID,
		Email:    id.Email,
		Name:     id.Name,
		PeerName: id.PeerName,
		PeerIP:   id.PeerIP,
		Groups:   id.Groups,
	}
}
