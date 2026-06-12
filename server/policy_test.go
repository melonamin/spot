package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccessPolicyAllows(t *testing.T) {
	sasha := Identity{Email: "sasha@corp.com", Groups: []string{"team-payments", "All"}}
	tests := []struct {
		name  string
		allow []string
		id    Identity
		want  bool
	}{
		{"email match", []string{"sasha@corp.com"}, sasha, true},
		{"email case-insensitive", []string{"Sasha@Corp.COM"}, sasha, true},
		{"group match", []string{"team-payments"}, sasha, true},
		{"group case-insensitive", []string{"Team-Payments"}, sasha, true},
		{"no match", []string{"other@corp.com", "team-infra"}, sasha, false},
		{"empty list denies", []string{}, sasha, false},
		{"blank entries ignored", []string{"", "  "}, sasha, false},
		{"empty email never matches", []string{"@"}, Identity{}, false},
		{"identity without groups", []string{"team-payments"}, Identity{Email: "x@corp.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &AccessPolicy{Allow: tt.allow}
			if got := policy.Allows(tt.id); got != tt.want {
				t.Errorf("Allows(%+v) with allow=%v = %v, want %v", tt.id, tt.allow, got, tt.want)
			}
		})
	}
}

func writeSiteFile(t *testing.T, dir, site, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, site), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, site, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyStore(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "restricted", accessFileName, `{"allow": ["sasha@corp.com"]}`)
	writeSiteFile(t, dir, "broken", accessFileName, `{not json`)
	writeSiteFile(t, dir, "open", "index.html", "<h1>hi</h1>")

	store := NewPolicyStore(dir, 0)

	policy, err := store.For("restricted")
	if err != nil || policy == nil {
		t.Fatalf("For(restricted) = %v, %v; want policy", policy, err)
	}
	if len(policy.Allow) != 1 || policy.Allow[0] != "sasha@corp.com" {
		t.Errorf("For(restricted).Allow = %v", policy.Allow)
	}

	policy, err = store.For("open")
	if err != nil || policy != nil {
		t.Errorf("For(open) = %v, %v; want nil, nil (default open)", policy, err)
	}
	policy, err = store.For("nonexistent")
	if err != nil || policy != nil {
		t.Errorf("For(nonexistent) = %v, %v; want nil, nil (default open)", policy, err)
	}

	if _, err = store.For("broken"); err == nil {
		t.Error("For(broken): want parse error so callers fail closed, got nil")
	}
}

func TestPolicyStoreCaches(t *testing.T) {
	dir := t.TempDir()
	store := NewPolicyStore(dir, time.Hour)

	if policy, err := store.For("late"); policy != nil || err != nil {
		t.Fatalf("For(late) before deploy = %v, %v", policy, err)
	}
	// The open verdict was cached; a new policy file must not be picked
	// up within the TTL.
	writeSiteFile(t, dir, "late", accessFileName, `{"allow": []}`)
	if policy, _ := store.For("late"); policy != nil {
		t.Error("For(late) within TTL: want cached nil policy")
	}
}

// authzServer builds a Server whose policies come from a temp dir and
// whose identity comes from the stub NetBird API.
func authzServer(t *testing.T, dir string) *Server {
	t.Helper()
	requests := 0
	api := newNetbirdAPI(t, &requests)
	t.Cleanup(api.Close)
	return &Server{
		policies:   NewPolicyStore(dir, 0),
		resolver:   NewNetbirdResolver(api.URL, "test-token", time.Minute),
		spotDomain: "spot.localhost",
	}
}

func TestHandleAuthz(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	writeSiteFile(t, dir, "bygroup", accessFileName, `{"allow": ["engineering"]}`)
	writeSiteFile(t, dir, "broken", accessFileName, `{not json`)
	srv := authzServer(t, dir)

	authz := func(host, peerIP string) int {
		req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/authz", nil)
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Forwarded-For", peerIP)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	tests := []struct {
		name string
		host string
		ip   string
		want int
	}{
		{"open site, unknown peer", "anything.spot.localhost", "10.0.0.1", 200},
		{"apex always open", "spot.localhost", "10.0.0.1", 200},
		{"allowed by email", "secret.spot.localhost", "100.64.0.7", 200},
		{"denied", "secret.spot.localhost", "100.64.0.9", 403},
		{"unknown peer denied", "secret.spot.localhost", "10.9.9.9", 403},
		{"allowed by group", "bygroup.spot.localhost", "100.64.0.7", 200},
		{"broken policy fails closed", "broken.spot.localhost", "100.64.0.7", 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authz(tt.host, tt.ip); got != tt.want {
				t.Errorf("authz(%s from %s) = %d, want %d", tt.host, tt.ip, got, tt.want)
			}
		})
	}
}

func TestHandleAuthzRestrictedWithoutResolver(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	srv := &Server{policies: NewPolicyStore(dir, 0), spotDomain: "spot.localhost"}

	req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/authz", nil)
	req.Header.Set("X-Forwarded-Host", "secret.spot.localhost")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("restricted site without resolver = %d, want 503 (fail closed)", rec.Code)
	}

	req.Header.Set("X-Forwarded-Host", "open.spot.localhost")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("open site without resolver = %d, want 200 (default open)", rec.Code)
	}
}
