package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const notifyChannel = "spot_docs"

// docChange is the NOTIFY payload: coordinates only, never document
// data, since NOTIFY payloads are capped at 8000 bytes and documents
// are not. The listener fetches bodies on delivery.
type docChange struct {
	Action     string `json:"action"`
	Scope      string `json:"scope"`
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

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

// Listener relays Postgres NOTIFY events to the hub. It holds one
// dedicated connection and reconnects with backoff if it drops.
type Listener struct {
	dsn   string
	store *DocStore
	hub   *Hub
}

func (l *Listener) Run(ctx context.Context) {
	for {
		if err := l.listen(ctx); err != nil && ctx.Err() == nil {
			log.Printf("realtime: listener: %v (reconnecting)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (l *Listener) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, l.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		l.dispatch(ctx, notification.Payload)
	}
}

func (l *Listener) dispatch(ctx context.Context, payload string) {
	var change docChange
	if err := json.Unmarshal([]byte(payload), &change); err != nil {
		log.Printf("realtime: bad notify payload %q: %v", payload, err)
		return
	}
	ev := Event{Type: change.Action, Collection: change.Collection, ID: change.ID}
	if change.Action != "delete" {
		doc, err := l.store.Get(ctx, change.Scope, change.Collection, change.ID)
		if errors.Is(err, ErrNotFound) {
			return // deleted between notify and fetch; the delete event follows
		}
		if err != nil {
			log.Printf("realtime: fetch %s/%s/%s: %v", change.Scope, change.Collection, change.ID, err)
			return
		}
		ev.Doc = &doc
	}
	l.hub.Publish(change.Scope, change.Collection, ev)
}
