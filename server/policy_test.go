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

func TestAccessPolicyAccessAndDownloadDefaults(t *testing.T) {
	if !(&AccessPolicy{Allow: []string{"team-payments"}}).RestrictsAccess() {
		t.Fatal("programmatic policy with Allow entries should restrict access")
	}

	tests := []struct {
		name             string
		raw              string
		wantRestricted   bool
		wantAllowCount   int
		wantDownloadable bool
	}{
		{
			name:             "empty policy is public and downloadable",
			raw:              `{}`,
			wantDownloadable: true,
		},
		{
			name:             "download opt-out alone keeps site public",
			raw:              `{"download": false}`,
			wantDownloadable: false,
		},
		{
			name:             "empty allow still denies everyone",
			raw:              `{"allow": []}`,
			wantRestricted:   true,
			wantDownloadable: true,
		},
		{
			name:             "restricted site can also disable downloads",
			raw:              `{"allow": ["team-payments"], "download": false}`,
			wantRestricted:   true,
			wantAllowCount:   1,
			wantDownloadable: false,
		},
		{
			name:             "capitalized allow remains restrictive",
			raw:              `{"Allow": ["team-payments"]}`,
			wantRestricted:   true,
			wantAllowCount:   1,
			wantDownloadable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := parseAccessPolicy("demo", []byte(tt.raw))
			if err != nil {
				t.Fatalf("parseAccessPolicy: %v", err)
			}
			if got := policy.RestrictsAccess(); got != tt.wantRestricted {
				t.Fatalf("RestrictsAccess = %v, want %v", got, tt.wantRestricted)
			}
			if got := len(policy.Allow); got != tt.wantAllowCount {
				t.Fatalf("len(Allow) = %d, want %d", got, tt.wantAllowCount)
			}
			if got := policy.AllowsDownload(); got != tt.wantDownloadable {
				t.Fatalf("AllowsDownload = %v, want %v", got, tt.wantDownloadable)
			}
		})
	}
}

func TestAccessPolicyRejectsNull(t *testing.T) {
	for _, raw := range []string{`null`, `{"allow": null}`, `{"alow": ["team-payments"]}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseAccessPolicy("demo", []byte(raw)); err == nil {
				t.Fatal("parseAccessPolicy succeeded, want fail-closed error")
			}
		})
	}
}

func TestAccessPolicyAIValues(t *testing.T) {
	for _, raw := range []string{`{}`, `{"ai":""}`, `{"ai":"owners"}`, `{"ai":"visitors"}`} {
		t.Run("valid/"+raw, func(t *testing.T) {
			if _, err := parseAccessPolicy("demo", []byte(raw)); err != nil {
				t.Fatalf("parseAccessPolicy(%s) = %v, want ok", raw, err)
			}
		})
	}
	// A typo like "visitor" must fail the parse so the policy fails closed
	// instead of silently behaving owner-only.
	for _, raw := range []string{`{"ai":"visitor"}`, `{"ai":"all"}`, `{"ai":"everyone"}`} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			if _, err := parseAccessPolicy("demo", []byte(raw)); err == nil {
				t.Fatalf("parseAccessPolicy(%s) succeeded, want fail-closed error", raw)
			}
		})
	}
}

func writeSiteFile(t *testing.T, dir, site, name, content string) {
	t.Helper()
	full := filepath.Join(dir, site, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
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

func TestPolicyStoreSetAndInvalidate(t *testing.T) {
	dir := t.TempDir()
	store := NewPolicyStore(dir, time.Hour)

	if policy, err := store.For("demo"); policy != nil || err != nil {
		t.Fatalf("For(demo) before deploy = %v, %v", policy, err)
	}

	store.Set("demo", &AccessPolicy{Allow: []string{"sasha@example.com"}}, nil)
	policy, err := store.For("demo")
	if err != nil || policy == nil || !policy.Allows(Identity{Email: "sasha@example.com"}) {
		t.Fatalf("For(demo) after Set = %v, %v; want allowing policy", policy, err)
	}

	store.Set("demo", nil, nil)
	policy, err = store.For("demo")
	if err != nil || policy != nil {
		t.Fatalf("For(demo) after open Set = %v, %v; want open", policy, err)
	}

	store.Set("demo", nil, os.ErrPermission)
	if _, err = store.For("demo"); err == nil {
		t.Fatal("For(demo) after error Set: want fail-closed error")
	}

	writeSiteFile(t, dir, "demo", accessFileName, `{"allow":["fresh@example.com"]}`)
	store.Invalidate("demo")
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.Allows(Identity{Email: "fresh@example.com"}) {
		t.Fatalf("For(demo) after Invalidate = %v, %v; want fresh file policy", policy, err)
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
		policies:       NewPolicyStore(dir, 0),
		resolver:       NewNetbirdResolver(api.URL, "test-token", time.Minute),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
}

func tailscaleAuthzServer(t *testing.T, dir string) *Server {
	t.Helper()
	requests := 0
	api := newTailscaleAPI(t, &requests, tailscaleAPIFixture{})
	t.Cleanup(api.Close)
	return &Server{
		policies:       NewPolicyStore(dir, 0),
		resolver:       NewTailscaleResolver(api.URL, "test-token", "", time.Minute),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
}

func TestHandleAuthz(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	writeSiteFile(t, dir, "bygroup", accessFileName, `{"allow": ["engineering"]}`)
	writeSiteFile(t, dir, "nodownload", accessFileName, `{"download": false}`)
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
		{"download opt-out still public", "nodownload.spot.localhost", "10.0.0.1", 200},
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

func TestHandleAuthzTailscaleGroup(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "bygroup", accessFileName, `{"allow": ["team-payments"]}`)
	srv := tailscaleAuthzServer(t, dir)

	authz := func(peerIP string) int {
		req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/authz", nil)
		req.Header.Set("X-Forwarded-Host", "bygroup.spot.localhost")
		req.Header.Set("X-Forwarded-For", peerIP)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := authz("100.64.0.7"); got != http.StatusOK {
		t.Fatalf("Tailscale group member authz = %d, want 200", got)
	}
	if got := authz("100.64.0.9"); got != http.StatusForbidden {
		t.Fatalf("Tailscale non-member authz = %d, want 403", got)
	}
}

func TestHandleAuthzRestrictedWithoutResolver(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	srv := &Server{
		policies:       NewPolicyStore(dir, 0),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}

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

func TestRestrictedSiteDBAPICannotBypassCaddy(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	srv := authzServer(t, dir)

	// This models a direct request to spot-api with a chosen Host header,
	// not a Caddy forward_auth subrequest. The backend must still apply
	// the site's policy before touching the document store.
	req := httptest.NewRequest(http.MethodGet, "http://secret.spot.localhost/api/db/posts", nil)
	req.RemoteAddr = "100.64.0.9:12345"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("direct restricted DB request = %d, want 403", rec.Code)
	}
}

func TestRestrictedFileDownloadsCheckSitePolicy(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	srv := authzServer(t, dir)
	srv.files = failingFileStore{}

	denied := httptest.NewRequest(http.MethodGet,
		"http://spot-api/api/files/secret/00000000000000000000000000000000/report.txt", nil)
	denied.Header.Set("X-Forwarded-Host", "secret.spot.localhost")
	denied.Header.Set("X-Forwarded-For", "100.64.0.9")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, denied)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted file download by denied peer = %d, want 403", rec.Code)
	}

	allowed := httptest.NewRequest(http.MethodGet,
		"http://spot-api/api/files/secret/00000000000000000000000000000000/report.txt", nil)
	allowed.Header.Set("X-Forwarded-Host", "secret.spot.localhost")
	allowed.Header.Set("X-Forwarded-For", "100.64.0.7")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, allowed)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("allowed peer should pass policy and hit the unavailable store: got %d, want 500", rec.Code)
	}
}

// TestSiteAccessFailsClosedWithoutStore pins the fail-closed behavior of
// policyForSite: a server with neither a policy store nor a site store
// cannot read any site's _access.json, so access is denied rather than
// every site being treated as open.
func TestSiteAccessFailsClosedWithoutStore(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost"}
	req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/authz", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz with no policy or site store = %d, want 503 (fail closed)", rec.Code)
	}
}
