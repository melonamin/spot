package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity is what the mesh knows about a visitor: the owner of the
// NetBird peer the request came from.
type Identity struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	PeerName string `json:"peer_name"`
	PeerIP   string `json:"peer_ip"`
}

// NetbirdResolver maps WireGuard peer IPs to users via the NetBird
// management API, caching the peer and user lists for ttl.
type NetbirdResolver struct {
	apiURL string
	token  string
	ttl    time.Duration
	client *http.Client

	mu        sync.Mutex
	byIP      map[string]Identity
	fetchedAt time.Time
}

func NewNetbirdResolver(apiURL, token string, ttl time.Duration) *NetbirdResolver {
	return &NetbirdResolver{
		apiURL: strings.TrimSuffix(apiURL, "/"),
		token:  token,
		ttl:    ttl,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *NetbirdResolver) Resolve(ctx context.Context, ip string) (Identity, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.fetchedAt) >= r.ttl {
		byIP, err := r.fetch(ctx)
		if err != nil {
			return Identity{}, false, err
		}
		r.byIP = byIP
		r.fetchedAt = time.Now()
	}
	id, ok := r.byIP[ip]
	return id, ok, nil
}

type netbirdPeer struct {
	IP     string `json:"ip"`
	Name   string `json:"name"`
	UserID string `json:"user_id"`
}

type netbirdUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (r *NetbirdResolver) fetch(ctx context.Context) (map[string]Identity, error) {
	var peers []netbirdPeer
	if err := r.get(ctx, "/api/peers", &peers); err != nil {
		return nil, err
	}
	var users []netbirdUser
	if err := r.get(ctx, "/api/users", &users); err != nil {
		return nil, err
	}

	usersByID := make(map[string]netbirdUser, len(users))
	for _, u := range users {
		usersByID[u.ID] = u
	}

	byIP := make(map[string]Identity, len(peers))
	for _, p := range peers {
		id := Identity{PeerName: p.Name, PeerIP: p.IP}
		if u, ok := usersByID[p.UserID]; ok {
			id.Email = u.Email
			id.Name = u.Name
		}
		byIP[p.IP] = id
	}
	return byIP, nil
}

func (r *NetbirdResolver) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL+path, nil)
	if err != nil {
		return fmt.Errorf("netbird request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Token "+r.token)
	req.Header.Set("Accept", "application/json")

	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("netbird request %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("netbird request %s: unexpected status %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("netbird response %s: %w", path, err)
	}
	return nil
}

// clientIP returns the address the request originated from. Caddy is the
// only thing in front of this server and appends the real client to
// X-Forwarded-For; fall back to the socket address when absent.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
