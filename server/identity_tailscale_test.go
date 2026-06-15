package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type tailscaleAPIFixture struct {
	status      int
	badJSONPath string
}

func newTailscaleAPI(t *testing.T, requests *int, fixture tailscaleAPIFixture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		*requests++
		w.Header().Set("Content-Type", "application/json")
		if fixture.status != 0 {
			w.WriteHeader(fixture.status)
			return
		}
		if r.URL.Path == fixture.badJSONPath {
			w.Write([]byte(`{not json`))
			return
		}
		switch r.URL.Path {
		case "/api/v2/tailnet/-/devices":
			json.NewEncoder(w).Encode(tailscaleDevicesResponse{Devices: []tailscaleDevice{
				{
					Addresses: []string{"100.64.0.7", "fd7a:115c:a1e0::7"},
					User:      "sasha@example.com",
					Hostname:  "sasha-laptop",
				},
				{
					Addresses: []string{"100.64.0.8"},
					User:      "sasha@example.com",
					Hostname:  "ci-runner",
					Tags:      []string{"tag:ci"},
				},
				{
					Addresses: []string{"100.64.0.9"},
					User:      "missing@example.com",
					Hostname:  "unknown-user",
				},
			}})
		case "/api/v2/tailnet/-/users":
			json.NewEncoder(w).Encode(tailscaleUsersResponse{Users: []tailscaleUser{
				{LoginName: "sasha@example.com", DisplayName: "Sasha"},
				{LoginName: "bob@example.com", DisplayName: "Bob"},
			}})
		case "/api/v2/tailnet/-/acl":
			json.NewEncoder(w).Encode(tailscaleACLResponse{Groups: map[string][]string{
				"group:team-payments": {"sasha@example.com", "autogroup:admin", "group:nested"},
				"group:Alpha":         {"bob@example.com"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTailscaleResolver(t *testing.T) {
	requests := 0
	api := newTailscaleAPI(t, &requests, tailscaleAPIFixture{})
	defer api.Close()

	r := NewTailscaleResolver(api.URL, "test-token", "", time.Minute)
	ctx := context.Background()

	id, found, err := r.Resolve(ctx, "100.64.0.7")
	if err != nil {
		t.Fatalf("Resolve IPv4: %v", err)
	}
	if !found {
		t.Fatal("Resolve IPv4: peer not found")
	}
	if id.Email != "sasha@example.com" || id.Name != "Sasha" || id.PeerName != "sasha-laptop" || id.PeerIP != "100.64.0.7" {
		t.Errorf("Resolve IPv4 = %+v", id)
	}
	wantGroups := []string{"group:team-payments", "team-payments"}
	if len(id.Groups) != len(wantGroups) || id.Groups[0] != wantGroups[0] || id.Groups[1] != wantGroups[1] {
		t.Errorf("Resolve IPv4 Groups = %v, want %v", id.Groups, wantGroups)
	}
	if !(&AccessPolicy{Allow: []string{"team-payments"}}).Allows(id) {
		t.Error("stripped Tailscale group did not match access policy")
	}
	if !(&AccessPolicy{Allow: []string{"group:team-payments"}}).Allows(id) {
		t.Error("prefixed Tailscale group did not match access policy")
	}

	id, found, err = r.Resolve(ctx, "fd7a:115c:a1e0::7")
	if err != nil || !found {
		t.Fatalf("Resolve IPv6: found=%v err=%v", found, err)
	}
	if id.PeerIP != "100.64.0.7" {
		t.Errorf("Resolve IPv6 PeerIP = %q, want canonical IPv4", id.PeerIP)
	}

	id, found, err = r.Resolve(ctx, "100.64.0.8")
	if err != nil || !found {
		t.Fatalf("Resolve tagged device: found=%v err=%v", found, err)
	}
	if id.Email != "" || id.Name != "" || id.PeerName != "ci-runner" {
		t.Errorf("tagged device identity = %+v, want peer-only", id)
	}

	id, found, err = r.Resolve(ctx, "100.64.0.9")
	if err != nil || !found {
		t.Fatalf("Resolve unmatched user: found=%v err=%v", found, err)
	}
	if id.Email != "" || id.Name != "" || id.PeerName != "unknown-user" {
		t.Errorf("unmatched user identity = %+v, want peer-only", id)
	}

	if _, found, _ = r.Resolve(ctx, "100.64.0.99"); found {
		t.Error("Resolve unknown IP: want not found")
	}

	if requests != 3 {
		t.Errorf("API requests = %d, want 3 (cached devices + users + acl)", requests)
	}
}

func TestTailscaleResolverIPv6TextualForm(t *testing.T) {
	requests := 0
	api := newTailscaleAPI(t, &requests, tailscaleAPIFixture{})
	defer api.Close()

	r := NewTailscaleResolver(api.URL, "test-token", "", time.Minute)
	ctx := context.Background()

	// The device advertises fd7a:115c:a1e0::7; an equal address in a
	// different textual form (expanded, uppercase) must still resolve.
	id, found, err := r.Resolve(ctx, "FD7A:115C:A1E0:0000:0000:0000:0000:0007")
	if err != nil {
		t.Fatalf("Resolve IPv6 textual form: %v", err)
	}
	if !found {
		t.Fatal("Resolve IPv6 textual form: peer not found")
	}
	if id.Email != "sasha@example.com" || id.PeerName != "sasha-laptop" {
		t.Errorf("Resolve IPv6 textual form = %+v", id)
	}
}

func TestTailscaleDirectory(t *testing.T) {
	requests := 0
	api := newTailscaleAPI(t, &requests, tailscaleAPIFixture{})
	defer api.Close()

	r := NewTailscaleResolver(api.URL, "test-token", "", time.Minute)
	dir, err := r.Directory(context.Background())
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	want := []AccessSuggestion{
		{Type: "user", Value: "bob@example.com", Label: "bob@example.com", Meta: "Bob"},
		{Type: "user", Value: "sasha@example.com", Label: "sasha@example.com", Meta: "Sasha"},
		{Type: "group", Value: "Alpha", Label: "Alpha", Meta: "Group"},
		{Type: "group", Value: "team-payments", Label: "team-payments", Meta: "Group"},
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

func TestNormalizeGroupsIgnoresNonEmailMembers(t *testing.T) {
	got := normalizeGroups(map[string][]string{
		"group:team": {"sasha@example.com", "autogroup:member", "group:other", "example.com"},
	})
	want := []string{"sasha@example.com"}
	members := got["group:team"]
	if len(members) != len(want) || members[0] != want[0] {
		t.Fatalf("normalizeGroups = %v, want %v", got, want)
	}
}

func TestTailscaleCanonicalAddress(t *testing.T) {
	tests := []struct {
		name      string
		addresses []string
		want      string
	}{
		{"cgnat IPv4 preferred", []string{"fd7a:115c:a1e0::7", "100.127.0.7"}, "100.127.0.7"},
		{"non-cgnat IPv4 ignored", []string{"192.0.2.7", "fd7a:115c:a1e0::7"}, "192.0.2.7"},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailscaleCanonicalAddress(tt.addresses); got != tt.want {
				t.Fatalf("tailscaleCanonicalAddress(%v) = %q, want %q", tt.addresses, got, tt.want)
			}
		})
	}
}

func TestTailscaleResolverErrors(t *testing.T) {
	tests := []struct {
		name    string
		fixture tailscaleAPIFixture
		want    string
	}{
		{"non-200", tailscaleAPIFixture{status: http.StatusInternalServerError}, "unexpected status 500"},
		{"malformed JSON", tailscaleAPIFixture{badJSONPath: "/api/v2/tailnet/-/devices"}, "tailscale response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			api := newTailscaleAPI(t, &requests, tt.fixture)
			defer api.Close()

			r := NewTailscaleResolver(api.URL, "test-token", "", time.Minute)
			_, _, err := r.Resolve(context.Background(), "100.64.0.7")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func newTailscaleOAuthAPI(t *testing.T, tokenStatus int, tokenRequests, dataRequests *int) *httptest.Server {
	t.Helper()
	currentToken := ""
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/oauth/token" {
			*tokenRequests++
			if tokenStatus != 0 {
				w.WriteHeader(tokenStatus)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			clientID, clientSecret, ok := r.BasicAuth()
			if !ok || clientID != "client-id" || clientSecret != "client-secret" {
				t.Errorf("oauth basic auth = %q/%q ok=%v", clientID, clientSecret, ok)
			}
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "" || r.Form.Get("client_secret") != "" {
				t.Errorf("oauth form = %v", r.Form)
			}
			currentToken = "minted-" + strconv.Itoa(*tokenRequests)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": currentToken,
				"expires_in":   61,
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+currentToken || currentToken == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		*dataRequests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/tailnet/-/devices":
			json.NewEncoder(w).Encode(tailscaleDevicesResponse{Devices: []tailscaleDevice{
				{Addresses: []string{"100.64.0.7"}, User: "sasha@example.com", Hostname: "sasha-laptop"},
			}})
		case "/api/v2/tailnet/-/users":
			json.NewEncoder(w).Encode(tailscaleUsersResponse{Users: []tailscaleUser{
				{LoginName: "sasha@example.com", DisplayName: "Sasha"},
			}})
		case "/api/v2/tailnet/-/acl":
			json.NewEncoder(w).Encode(tailscaleACLResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTailscaleOAuthTokenSourceRefreshes(t *testing.T) {
	tokenRequests := 0
	dataRequests := 0
	api := newTailscaleOAuthAPI(t, 0, &tokenRequests, &dataRequests)
	defer api.Close()

	r := NewTailscaleOAuthResolver(api.URL, "client-id", "client-secret", "", 0)
	id, found, err := r.Resolve(context.Background(), "100.64.0.7")
	if err != nil || !found || id.Email != "sasha@example.com" {
		t.Fatalf("first Resolve = %+v found=%v err=%v", id, found, err)
	}
	time.Sleep(1100 * time.Millisecond)
	id, found, err = r.Resolve(context.Background(), "100.64.0.7")
	if err != nil || !found || id.Email != "sasha@example.com" {
		t.Fatalf("second Resolve = %+v found=%v err=%v", id, found, err)
	}
	if tokenRequests != 2 {
		t.Fatalf("token requests = %d, want 2 after near-expiry refresh", tokenRequests)
	}
	if dataRequests != 6 {
		t.Fatalf("data requests = %d, want 6", dataRequests)
	}
}

func TestTailscaleOAuthShortLivedTokenNotInstantlyExpired(t *testing.T) {
	tokenRequests := 0
	currentToken := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/oauth/token" {
			tokenRequests++
			currentToken = "minted-" + strconv.Itoa(tokenRequests)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": currentToken,
				// Lifetime well below the 60s refresh skew: the token must
				// still be usable rather than treated as instantly expired.
				"expires_in": 10,
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+currentToken || currentToken == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/tailnet/-/devices":
			json.NewEncoder(w).Encode(tailscaleDevicesResponse{Devices: []tailscaleDevice{
				{Addresses: []string{"100.64.0.7"}, User: "sasha@example.com", Hostname: "sasha-laptop"},
			}})
		case "/api/v2/tailnet/-/users":
			json.NewEncoder(w).Encode(tailscaleUsersResponse{Users: []tailscaleUser{
				{LoginName: "sasha@example.com", DisplayName: "Sasha"},
			}})
		case "/api/v2/tailnet/-/acl":
			json.NewEncoder(w).Encode(tailscaleACLResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	// Cache the data so a second Resolve reuses the minted token instead of
	// refetching, isolating token reuse from the device cache TTL.
	r := NewTailscaleOAuthResolver(api.URL, "client-id", "client-secret", "", time.Minute)
	id, found, err := r.Resolve(context.Background(), "100.64.0.7")
	if err != nil || !found || id.Email != "sasha@example.com" {
		t.Fatalf("first Resolve = %+v found=%v err=%v", id, found, err)
	}
	if _, _, err := r.Resolve(context.Background(), "100.64.0.7"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if tokenRequests != 1 {
		t.Fatalf("token requests = %d, want 1 (short-lived token reused, not instantly expired)", tokenRequests)
	}
}

func TestTailscaleOAuthTokenError(t *testing.T) {
	tokenRequests := 0
	dataRequests := 0
	api := newTailscaleOAuthAPI(t, http.StatusUnauthorized, &tokenRequests, &dataRequests)
	defer api.Close()

	r := NewTailscaleOAuthResolver(api.URL, "client-id", "client-secret", "", time.Minute)
	_, _, err := r.Resolve(context.Background(), "100.64.0.7")
	if err == nil || !strings.Contains(err.Error(), "tailscale oauth token: unexpected status 401") {
		t.Fatalf("Resolve error = %v, want token status", err)
	}
	if dataRequests != 0 {
		t.Fatalf("data requests = %d, want 0 when token minting fails", dataRequests)
	}
}
