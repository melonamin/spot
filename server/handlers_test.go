package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("untrusted forwarded headers = %d, want 400", rec.Code)
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
