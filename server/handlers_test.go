package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSameOriginOnly(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t)}

	call := func(origin string) int {
		req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/me", nil)
		req.Header.Set("X-Forwarded-Host", "demo.spot.localhost")
		// The same-origin check matches scheme too, so the forwarded scheme
		// from the trusted proxy must agree with the https Origin below.
		req.Header.Set("X-Forwarded-Proto", "https")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("https://demo.spot.localhost"); got != http.StatusServiceUnavailable {
		t.Fatalf("same-origin API request = %d, want downstream 503", got)
	}
	if got := call(""); got != http.StatusServiceUnavailable {
		t.Fatalf("non-browser API request without Origin = %d, want downstream 503", got)
	}
	if got := call("https://evil.spot.localhost"); got != http.StatusForbidden {
		t.Fatalf("cross-site API request = %d, want 403", got)
	}
	if got := call("not a url"); got != http.StatusForbidden {
		t.Fatalf("bad Origin API request = %d, want 403", got)
	}
}

func TestRejectsForwardedHeadersFromUntrustedRemote(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost"}
	req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/me", nil)
	req.RemoteAddr = "198.51.100.9:12345"
	req.Header.Set("X-Forwarded-For", "100.64.0.7")
	req.Header.Set("X-Forwarded-Host", "demo.spot.localhost")
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("untrusted forwarded headers = %d, want 400", rec.Code)
	}
}

func TestRejectsUnknownHost(t *testing.T) {
	srv := &Server{
		spotDomain:  "spot.localhost",
		serveStatic: true,
	}
	for _, tt := range []struct {
		name string
		req  *http.Request
	}{
		{
			name: "api",
			req:  httptest.NewRequest(http.MethodGet, "http://attacker.example/api/me", nil),
		},
		{
			name: "static",
			req:  httptest.NewRequest(http.MethodGet, "http://attacker.example/", nil),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, tt.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("unknown host = %d %s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUnknownAPIDoesNotFallThroughToStatic(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "demo", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSiteFile(t, dir, "demo", "api/unknown", "site api file")
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		spotDomain:     "spot.localhost",
		sites:          sites,
		serveStatic:    true,
		trustedProxies: testTrustedProxies(t),
	}

	req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/unknown", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown api = %d %q, want 404", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "site api file") {
		t.Fatalf("unknown api served site content: %q", rec.Body.String())
	}
}

func TestRequestScheme(t *testing.T) {
	srv := &Server{trustedProxies: testTrustedProxies(t)}

	httpReq := httptest.NewRequest(http.MethodGet, "http://spot.localhost/", nil)
	if got := srv.requestScheme(httpReq); got != "http" {
		t.Fatalf("direct http scheme = %q, want http", got)
	}

	httpsReq := httptest.NewRequest(http.MethodGet, "https://spot.localhost/", nil)
	if got := srv.requestScheme(httpsReq); got != "https" {
		t.Fatalf("direct https scheme = %q, want https", got)
	}

	proxied := httptest.NewRequest(http.MethodGet, "http://spot-api/", nil)
	proxied.RemoteAddr = "192.0.2.10:12345"
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if got := srv.requestScheme(proxied); got != "https" {
		t.Fatalf("trusted forwarded scheme = %q, want https", got)
	}

	untrusted := httptest.NewRequest(http.MethodGet, "http://spot-api/", nil)
	untrusted.RemoteAddr = "198.51.100.10:12345"
	untrusted.Header.Set("X-Forwarded-Proto", "https")
	if got := srv.requestScheme(untrusted); got != "http" {
		t.Fatalf("untrusted forwarded scheme = %q, want direct http", got)
	}
}

func TestWebSocketRequiresSameOrigin(t *testing.T) {
	srv := &Server{
		hub:        NewHub(),
		spotDomain: "spot.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	dial := func(origin string) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{
				"X-Forwarded-Host": []string{"demo.spot.localhost"},
				"Origin":           []string{origin},
			},
		})
		if conn != nil {
			conn.CloseNow()
		}
		if res == nil {
			return 0, err
		}
		if res.Body != nil {
			res.Body.Close()
		}
		return res.StatusCode, err
	}

	if got, err := dial("http://demo.spot.localhost"); err != nil || got != http.StatusSwitchingProtocols {
		t.Fatalf("same-origin websocket = status %d, err %v; want 101", got, err)
	}
	if got, err := dial("http://evil.spot.localhost"); err == nil || got != http.StatusForbidden {
		t.Fatalf("cross-site websocket = status %d, err %v; want 403", got, err)
	}
}
