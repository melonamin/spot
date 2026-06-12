package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newNetbirdAPI serves the two NetBird management endpoints the resolver
// uses, counting requests so cache behavior can be asserted.
func newNetbirdAPI(t *testing.T, requests *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		*requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/peers":
			json.NewEncoder(w).Encode([]netbirdPeer{
				{IP: "100.64.0.7", Name: "sasha-laptop", UserID: "u1",
					Groups: []netbirdGroupRef{{ID: "g1", Name: "laptops"}}},
				{IP: "100.64.0.9", Name: "ci-runner", UserID: ""},
			})
		case "/api/users":
			json.NewEncoder(w).Encode([]netbirdUser{
				{ID: "u1", Email: "sasha@example.com", Name: "Sasha", AutoGroups: []string{"g2"}},
			})
		case "/api/groups":
			json.NewEncoder(w).Encode([]netbirdGroupRef{
				{ID: "g1", Name: "laptops"},
				{ID: "g2", Name: "engineering"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestNetbirdResolver(t *testing.T) {
	requests := 0
	api := newNetbirdAPI(t, &requests)
	defer api.Close()

	r := NewNetbirdResolver(api.URL, "test-token", time.Minute)
	ctx := context.Background()

	id, found, err := r.Resolve(ctx, "100.64.0.7")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !found {
		t.Fatal("Resolve(100.64.0.7): peer not found")
	}
	if id.Email != "sasha@example.com" || id.Name != "Sasha" || id.PeerName != "sasha-laptop" {
		t.Errorf("Resolve(100.64.0.7) = %+v", id)
	}
	// Peer groups and the owner's auto-groups are merged, sorted.
	if len(id.Groups) != 2 || id.Groups[0] != "engineering" || id.Groups[1] != "laptops" {
		t.Errorf("Resolve(100.64.0.7).Groups = %v, want [engineering laptops]", id.Groups)
	}

	// Peer registered with a setup key has no user behind it.
	id, found, err = r.Resolve(ctx, "100.64.0.9")
	if err != nil || !found {
		t.Fatalf("Resolve(100.64.0.9): found=%v err=%v", found, err)
	}
	if id.Email != "" || id.PeerName != "ci-runner" {
		t.Errorf("Resolve(100.64.0.9) = %+v", id)
	}

	if _, found, _ = r.Resolve(ctx, "100.64.0.99"); found {
		t.Error("Resolve(100.64.0.99): want not found")
	}

	// All three lookups must come from one fetch
	// (peers + users + groups = 3 requests).
	if requests != 3 {
		t.Errorf("API requests = %d, want 3 (cached)", requests)
	}
}

func TestNetbirdDirectory(t *testing.T) {
	requests := 0
	api := newNetbirdAPI(t, &requests)
	defer api.Close()

	r := NewNetbirdResolver(api.URL, "test-token", time.Minute)
	dir, err := r.Directory(context.Background())
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	// One user (by email) and two groups, users sorted before groups.
	want := []AccessSuggestion{
		{Type: "user", Value: "sasha@example.com", Label: "sasha@example.com", Meta: "Sasha"},
		{Type: "group", Value: "engineering", Label: "engineering", Meta: "Group"},
		{Type: "group", Value: "laptops", Label: "laptops", Meta: "Group"},
	}
	if len(dir) != len(want) {
		t.Fatalf("Directory = %+v, want %+v", dir, want)
	}
	for i := range want {
		if dir[i] != want[i] {
			t.Errorf("Directory[%d] = %+v, want %+v", i, dir[i], want[i])
		}
	}
}

func TestAccessSuggestionsEndpoint(t *testing.T) {
	requests := 0
	api := newNetbirdAPI(t, &requests)
	defer api.Close()
	srv := &Server{
		resolver:   NewNetbirdResolver(api.URL, "test-token", time.Minute),
		spotDomain: "spot.localhost",
	}

	suggest := func(host, query string) (int, []AccessSuggestion) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/access/suggestions?q="+query, nil)
		req.Host = host
		req.RemoteAddr = "100.64.0.7:40000" // a known peer, so identity resolves
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		var body struct {
			Suggestions []AccessSuggestion `json:"suggestions"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Suggestions
	}

	// A group match on the apex.
	code, got := suggest("spot.localhost", "laptop")
	if code != http.StatusOK || len(got) != 1 || got[0].Value != "laptops" || got[0].Type != "group" {
		t.Fatalf("q=laptop: code %d, got %+v", code, got)
	}
	// A user match by email/name.
	if code, got = suggest("spot.localhost", "sasha"); code != http.StatusOK || len(got) != 1 || got[0].Value != "sasha@example.com" {
		t.Fatalf("q=sasha: code %d, got %+v", code, got)
	}
	// Empty query returns nothing rather than dumping the directory.
	if code, got = suggest("spot.localhost", ""); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("empty q: code %d, got %+v", code, got)
	}
	// Apex-only: a site subdomain must not reach the directory.
	if code, _ = suggest("demo.spot.localhost", "sasha"); code != http.StatusBadRequest {
		t.Fatalf("site-host suggestions: code %d, want 400", code)
	}
}

func TestAccessSuggestionsWithoutDirectory(t *testing.T) {
	// The dev static resolver has identity but no directory: the endpoint
	// answers 200 with an empty list rather than erroring.
	srv := &Server{
		resolver:   NewStaticResolver("dev@example.com", "Dev", nil),
		spotDomain: "spot.localhost",
	}
	req := httptest.NewRequest(http.MethodGet, "http://spot.localhost/api/access/suggestions?q=any", nil)
	req.Host = "spot.localhost"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("static resolver suggestions: code %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"suggestions":[]`) {
		t.Errorf("static resolver suggestions body = %s, want empty list", rec.Body.String())
	}
}

func TestHandleMeUnconfigured(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost"}
	req := httptest.NewRequest(http.MethodGet, "http://mysite.spot.localhost/api/me", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/me without resolver: status %d, want 503", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	srv := &Server{trustedProxies: testTrustedProxies(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.5:39000"
	req.Header.Set("X-Forwarded-For", "100.64.0.7")
	if got := srv.clientIP(req); got != "172.18.0.5" {
		t.Errorf("clientIP ignores untrusted XFF = %q, want 172.18.0.5", got)
	}

	req.RemoteAddr = "192.0.2.1:39000"
	if got := srv.clientIP(req); got != "100.64.0.7" {
		t.Errorf("clientIP with XFF = %q, want 100.64.0.7", got)
	}

	// A client-supplied XFF that a proxy appended to must not win:
	// only the last entry (the proxy-set one) is trusted.
	req.Header.Set("X-Forwarded-For", "100.64.0.66, 100.64.0.7")
	if got := srv.clientIP(req); got != "100.64.0.7" {
		t.Errorf("clientIP with spoofed XFF prefix = %q, want 100.64.0.7", got)
	}
	req.Header.Add("X-Forwarded-For", "100.64.0.8")
	if got := srv.clientIP(req); got != "100.64.0.8" {
		t.Errorf("clientIP with multiple XFF headers = %q, want 100.64.0.8", got)
	}

	req.Header.Del("X-Forwarded-For")
	req.RemoteAddr = "172.18.0.5:39000"
	if got := srv.clientIP(req); got != "172.18.0.5" {
		t.Errorf("clientIP without XFF = %q, want 172.18.0.5", got)
	}
}
