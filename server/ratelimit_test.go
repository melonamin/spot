package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterBurstAndIsolation(t *testing.T) {
	limiter := NewRateLimiter(1, 3)

	for i := range 3 {
		if !limiter.Allow("100.64.0.7") {
			t.Fatalf("request %d within burst was denied", i+1)
		}
	}
	if limiter.Allow("100.64.0.7") {
		t.Error("request beyond burst was allowed")
	}
	// A different peer has its own bucket.
	if !limiter.Allow("100.64.0.9") {
		t.Error("other peer was denied by a stranger's bucket")
	}
}

func TestLimitedHandler(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	srv := &Server{trustedProxies: testTrustedProxies(t)}
	handler := srv.limited(limiter, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		req.Header.Set("X-Forwarded-For", "100.64.0.7")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("first call: status %d", rec.Code)
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
}
