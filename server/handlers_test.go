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
	srv := &Server{quickDomain: "quick.localhost"}

	call := func(origin string) int {
		req := httptest.NewRequest(http.MethodGet, "http://quick-api/api/me", nil)
		req.Header.Set("X-Forwarded-Host", "demo.quick.localhost")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("https://demo.quick.localhost"); got != http.StatusServiceUnavailable {
		t.Fatalf("same-origin API request = %d, want downstream 503", got)
	}
	if got := call(""); got != http.StatusServiceUnavailable {
		t.Fatalf("non-browser API request without Origin = %d, want downstream 503", got)
	}
	if got := call("https://evil.quick.localhost"); got != http.StatusForbidden {
		t.Fatalf("cross-site API request = %d, want 403", got)
	}
	if got := call("not a url"); got != http.StatusForbidden {
		t.Fatalf("bad Origin API request = %d, want 403", got)
	}
}

func TestWebSocketRequiresSameOrigin(t *testing.T) {
	srv := &Server{
		hub:         NewHub(),
		quickDomain: "quick.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	dial := func(origin string) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{
				"X-Forwarded-Host": []string{"demo.quick.localhost"},
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

	if got, err := dial("http://demo.quick.localhost"); err != nil || got != http.StatusSwitchingProtocols {
		t.Fatalf("same-origin websocket = status %d, err %v; want 101", got, err)
	}
	if got, err := dial("http://evil.quick.localhost"); err == nil || got != http.StatusForbidden {
		t.Fatalf("cross-site websocket = status %d, err %v; want 403", got, err)
	}
}
