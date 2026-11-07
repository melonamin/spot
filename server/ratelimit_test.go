package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestRateLimiterCapEnforced(t *testing.T) {
	limiter := NewRateLimiter(1, 1)

	// A flood of distinct keys, all seen within the idle window so prune
	// cannot free anything, must never grow the map past the cap.
	for i := 0; i < maxVisitors*3; i++ {
		limiter.Allow(fmt.Sprintf("100.64.%d.%d", i/256, i%256))
	}

	limiter.mu.Lock()
	size := len(limiter.visitors)
	limiter.mu.Unlock()
	if size > maxVisitors {
		t.Fatalf("visitor map grew to %d, exceeds cap %d", size, maxVisitors)
	}
}

func TestRateLimiterPruneRemovesIdle(t *testing.T) {
	limiter := NewRateLimiter(1, 1)

	limiter.Allow("100.64.0.1")
	limiter.Allow("100.64.0.2")

	// Backdate one visitor past the idle cutoff so prune drops only it.
	limiter.mu.Lock()
	limiter.visitors["100.64.0.1"].lastSeen = time.Now().Add(-11 * time.Minute)
	limiter.prune()
	_, idleKept := limiter.visitors["100.64.0.1"]
	_, freshKept := limiter.visitors["100.64.0.2"]
	limiter.mu.Unlock()

	if idleKept {
		t.Error("prune kept an idle visitor")
	}
	if !freshKept {
		t.Error("prune dropped a fresh visitor")
	}
}

func TestLimitedHandlerExemptsSecretProvenForwardAuth(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	fa := NewForwardAuth("", "", "", "")
	fa.Secret = "0123456789abcdef0123456789abcdef"
	srv := &Server{forwardAuth: fa, trustedProxies: testTrustedProxies(t)}
	handler := srv.limited(limiter, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func(email, secret string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		req.Header.Set("Remote-Email", email)
		req.Header.Set(forwardAuthSecretHeader, secret)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	for i := 0; i < 5; i++ {
		if code := call("a@example.com", fa.Secret); code != http.StatusOK {
			t.Fatalf("proven call %d: status %d, want 200", i, code)
		}
	}
	// A wrong secret proves nothing, so the address bucket applies.
	if code := call("c@example.com", "wrong-secret-wrong-secret-wrong!"); code != http.StatusOK {
		t.Fatalf("first unproven call: status %d", code)
	}
	if code := call("d@example.com", "wrong-secret-wrong-secret-wrong!"); code != http.StatusTooManyRequests {
		t.Fatalf("second unproven call from the same address: status %d, want 429", code)
	}
}

// AI and Slack spend server-side credentials, so a secret-proven proxy gets a
// bucket per asserted user there instead of an exemption.
func TestLimitedSpendKeysSecretProvenForwardAuthByUser(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	fa := NewForwardAuth("", "", "", "")
	fa.Secret = "0123456789abcdef0123456789abcdef"
	srv := &Server{forwardAuth: fa, trustedProxies: testTrustedProxies(t)}
	handler := srv.limitedSpend(limiter, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	call := func(email string) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		req.Header.Set("Remote-Email", email)
		req.Header.Set(forwardAuthSecretHeader, fa.Secret)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	if code := call("a@example.com"); code != http.StatusOK {
		t.Fatalf("first call for a: status %d", code)
	}
	if code := call("a@example.com"); code != http.StatusTooManyRequests {
		t.Fatalf("second call for a: status %d, want 429", code)
	}
	// Another user behind the same proxy has its own bucket.
	if code := call("b@example.com"); code != http.StatusOK {
		t.Fatalf("first call for b: status %d, want 200", code)
	}
}
