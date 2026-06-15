package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestParseForwardGroups(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		want   []string
	}{
		{"none", nil, []string{}},
		{"single", []string{"engineering"}, []string{"engineering"}},
		{"comma separated", []string{"a, b ,c"}, []string{"a", "b", "c"}},
		{"repeated header", []string{"a", "b"}, []string{"a", "b"}},
		{"dedup", []string{"ops", "ops,ops"}, []string{"ops"}},
		{"drops blanks", []string{" , ", ""}, []string{}},
		{"preserves case", []string{"Admin,user"}, []string{"Admin", "user"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := parseForwardGroups(tt.values)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseForwardGroups(%q) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}

func TestForwardAuthIdentityFrom(t *testing.T) {
	fa := NewForwardAuth("", "", "", "") // defaults: Remote-User/Email/Name/Groups

	for _, tt := range []struct {
		name    string
		headers map[string]string
		wantOK  bool
		want    Identity
	}{
		{
			name:    "no identity headers",
			headers: map[string]string{"Remote-Name": "Nobody"},
			wantOK:  false,
		},
		{
			name: "full identity, email normalized",
			headers: map[string]string{
				"Remote-User":   "user_123",
				"Remote-Email":  "  Alice@Corp.COM ",
				"Remote-Name":   "Alice Example",
				"Remote-Groups": "engineering, ops",
			},
			wantOK: true,
			want: Identity{
				Email:    "alice@corp.com",
				Name:     "Alice Example",
				PeerName: "user_123",
				PeerIP:   "100.64.0.5",
				Groups:   []string{"engineering", "ops"},
			},
		},
		{
			name:    "user only, no email",
			headers: map[string]string{"Remote-User": "svc-deployer"},
			wantOK:  true,
			want: Identity{
				PeerName: "svc-deployer",
				PeerIP:   "100.64.0.5",
				Groups:   []string{},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got, ok := fa.identityFrom(h, "100.64.0.5")
			if ok != tt.wantOK {
				t.Fatalf("identityFrom ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identityFrom = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestForwardAuthCustomHeaders(t *testing.T) {
	fa := NewForwardAuth("X-User", "X-Email", "X-Name", "X-Groups")
	h := http.Header{}
	h.Set("X-Email", "bob@corp.com")
	h.Set("Remote-Email", "attacker@evil.com") // default name must be ignored
	id, ok := fa.identityFrom(h, "10.0.0.1")
	if !ok {
		t.Fatal("identityFrom: ok = false, want true")
	}
	if id.Email != "bob@corp.com" {
		t.Fatalf("email = %q, want bob@corp.com (custom header only)", id.Email)
	}
}

// TestForwardAuthTrustGate is the security-critical case: forward-auth
// headers are honored from a trusted proxy and ignored from an untrusted
// socket, so a client cannot impersonate a user by setting Remote-* itself.
func TestForwardAuthTrustGate(t *testing.T) {
	srv := &Server{
		forwardAuth:    NewForwardAuth("", "", "", ""),
		trustedProxies: testTrustedProxies(t),
	}

	newReq := func(remoteAddr string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("Remote-Email", "alice@corp.com")
		return req
	}

	t.Run("trusted proxy is honored", func(t *testing.T) {
		rec := httptest.NewRecorder()
		id, ok := srv.resolveIdentity(rec, newReq("192.0.2.10:443"), "test")
		if !ok {
			t.Fatalf("resolveIdentity from trusted proxy = not ok (%d)", rec.Code)
		}
		if id.Email != "alice@corp.com" {
			t.Fatalf("email = %q, want alice@corp.com", id.Email)
		}
	})

	t.Run("untrusted socket is ignored", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if _, ok := srv.resolveIdentity(rec, newReq("198.51.100.9:54321"), "test"); ok {
			t.Fatal("resolveIdentity from untrusted socket = ok, want rejected")
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("untrusted forward-auth = %d, want 404 (no identity)", rec.Code)
		}
	})
}

// TestForwardAuthPrecedence checks that proxy identity wins when present and
// the mesh resolver still answers requests that carry no forward-auth header.
func TestForwardAuthPrecedence(t *testing.T) {
	srv := &Server{
		forwardAuth:    NewForwardAuth("", "", "", ""),
		resolver:       NewStaticResolver("mesh@corp.com", "Mesh User", nil),
		trustedProxies: testTrustedProxies(t),
	}

	withHeader := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy", nil)
	withHeader.RemoteAddr = "192.0.2.10:443"
	withHeader.Header.Set("Remote-Email", "alice@corp.com")
	rec := httptest.NewRecorder()
	id, ok := srv.resolveIdentity(rec, withHeader, "test")
	if !ok || id.Email != "alice@corp.com" {
		t.Fatalf("with forward-auth header: ok=%v email=%q, want alice@corp.com", ok, id.Email)
	}

	noHeader := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy", nil)
	noHeader.RemoteAddr = "192.0.2.10:443"
	rec = httptest.NewRecorder()
	id, ok = srv.resolveIdentity(rec, noHeader, "test")
	if !ok || id.Email != "mesh@corp.com" {
		t.Fatalf("without forward-auth header: ok=%v email=%q, want mesh fallback", ok, id.Email)
	}
}

// TestForwardAuthSiteAccess covers the visitor gate (authorizeSiteAccess),
// which fronts restricted static files, the db/files APIs, and realtime when
// Spot sits behind an auth proxy such as Pangolin. A restricted site admits a
// proxy-asserted visitor by email or group, denies strangers, and ignores
// Remote-* from an untrusted socket.
func TestForwardAuthSiteAccess(t *testing.T) {
	policies := NewPolicyStore(t.TempDir(), time.Minute)
	policies.Set("locked", &AccessPolicy{Allow: []string{"alice@corp.com", "ops"}}, nil)
	srv := &Server{
		forwardAuth:    NewForwardAuth("", "", "", ""),
		policies:       policies,
		trustedProxies: testTrustedProxies(t),
	}

	access := func(remoteAddr string, headers map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, "http://locked.spot.localhost/api/authz", nil)
		req.RemoteAddr = remoteAddr
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.authorizeSiteAccess(rec, req, "locked")
		return rec.Code
	}

	for _, tt := range []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       int
	}{
		{"allowed by email", "192.0.2.10:443", map[string]string{"Remote-Email": "alice@corp.com"}, http.StatusOK},
		{"allowed by group", "192.0.2.10:443", map[string]string{"Remote-Email": "carol@corp.com", "Remote-Groups": "ops"}, http.StatusOK},
		{"stranger denied", "192.0.2.10:443", map[string]string{"Remote-Email": "stranger@corp.com"}, http.StatusForbidden},
		{"untrusted ignored", "198.51.100.9:54321", map[string]string{"Remote-Email": "alice@corp.com"}, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := access(tt.remoteAddr, tt.headers); got != tt.want {
				t.Fatalf("authorizeSiteAccess = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestForwardAuthDeployIdentity exercises the actual deploy seam: a forward
// -auth identity is accepted by requireDeployIdentity (it has a stable key).
func TestForwardAuthDeployIdentity(t *testing.T) {
	srv := &Server{
		forwardAuth:    NewForwardAuth("", "", "", ""),
		trustedProxies: testTrustedProxies(t),
	}
	req := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy", nil)
	req.RemoteAddr = "192.0.2.10:443"
	req.Header.Set("Remote-Email", "deployer@corp.com")
	req.Header.Set("Remote-Groups", "deployers")

	rec := httptest.NewRecorder()
	id, ok := srv.requireDeployIdentity(rec, req)
	if !ok {
		t.Fatalf("requireDeployIdentity = not ok (%d %s)", rec.Code, rec.Body.String())
	}
	if actorKey(id) != "deployer@corp.com" {
		t.Fatalf("actorKey = %q, want deployer@corp.com", actorKey(id))
	}
}
