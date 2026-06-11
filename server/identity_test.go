package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
				{IP: "100.64.0.7", Name: "sasha-laptop", UserID: "u1"},
				{IP: "100.64.0.9", Name: "ci-runner", UserID: ""},
			})
		case "/api/users":
			json.NewEncoder(w).Encode([]netbirdUser{
				{ID: "u1", Email: "sasha@example.com", Name: "Sasha"},
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

	// All three lookups must come from one fetch (peers + users = 2 requests).
	if requests != 2 {
		t.Errorf("API requests = %d, want 2 (cached)", requests)
	}
}

func TestHandleMeUnconfigured(t *testing.T) {
	srv := &Server{quickDomain: "quick.localhost"}
	req := httptest.NewRequest(http.MethodGet, "http://mysite.quick.localhost/api/me", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/me without resolver: status %d, want 503", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.5:39000"
	req.Header.Set("X-Forwarded-For", "100.64.0.7, 172.18.0.2")
	if got := clientIP(req); got != "100.64.0.7" {
		t.Errorf("clientIP with XFF = %q, want 100.64.0.7", got)
	}

	req.Header.Del("X-Forwarded-For")
	if got := clientIP(req); got != "172.18.0.5" {
		t.Errorf("clientIP without XFF = %q, want 172.18.0.5", got)
	}
}
