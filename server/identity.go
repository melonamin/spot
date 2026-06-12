package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Identity is what the mesh knows about a visitor: the owner of the
// NetBird peer the request came from. Groups is the union of the peer's
// groups and the owner's auto-groups, by name.
type Identity struct {
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	PeerName string   `json:"peer_name"`
	PeerIP   string   `json:"peer_ip"`
	Groups   []string `json:"groups"`
}

type IdentityResolver interface {
	Resolve(ctx context.Context, ip string) (Identity, bool, error)
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
	IP     string            `json:"ip"`
	Name   string            `json:"name"`
	UserID string            `json:"user_id"`
	Groups []netbirdGroupRef `json:"groups"`
}

type netbirdGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type netbirdUser struct {
	ID         string   `json:"id"`
	Email      string   `json:"email"`
	Name       string   `json:"name"`
	AutoGroups []string `json:"auto_groups"`
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
	var groups []netbirdGroupRef
	if err := r.get(ctx, "/api/groups", &groups); err != nil {
		return nil, err
	}

	usersByID := make(map[string]netbirdUser, len(users))
	for _, u := range users {
		usersByID[u.ID] = u
	}
	groupNames := make(map[string]string, len(groups))
	for _, g := range groups {
		groupNames[g.ID] = g.Name
	}

	byIP := make(map[string]Identity, len(peers))
	for _, p := range peers {
		id := Identity{PeerName: p.Name, PeerIP: p.IP}
		names := map[string]struct{}{}
		for _, g := range p.Groups {
			if g.Name != "" {
				names[g.Name] = struct{}{}
			}
		}
		if u, ok := usersByID[p.UserID]; ok {
			id.Email = u.Email
			id.Name = u.Name
			for _, gid := range u.AutoGroups {
				if name := groupNames[gid]; name != "" {
					names[name] = struct{}{}
				}
			}
		}
		id.Groups = make([]string, 0, len(names))
		for name := range names {
			id.Groups = append(id.Groups, name)
		}
		sort.Strings(id.Groups)
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

// StaticResolver is an explicit local-development identity provider. It
// is only enabled when configured; production should use NetbirdResolver.
type StaticResolver struct {
	identity Identity
}

func NewStaticResolver(email, name string, groups []string) *StaticResolver {
	return &StaticResolver{identity: Identity{
		Email:    email,
		Name:     name,
		PeerName: "static-dev",
		Groups:   groups,
	}}
}

func (r *StaticResolver) Resolve(_ context.Context, ip string) (Identity, bool, error) {
	id := r.identity
	id.PeerIP = ip
	return id, true, nil
}

// clientIP returns the address the request originated from. Identity
// hangs off this value, so it must not be spoofable: forwarded headers
// are only read when the socket peer is a trusted proxy. When present,
// the LAST X-Forwarded-For entry wins because Caddy overwrites it with
// the connection's address.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustsRemote(r) {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			entries := strings.Split(vals[len(vals)-1], ",")
			return strings.TrimSpace(entries[len(entries)-1])
		}
	}
	return remoteHost(r.RemoteAddr)
}
