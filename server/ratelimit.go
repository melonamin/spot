package main

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter applies a per-client token bucket. Clients are keyed by
// peer IP — the mesh guarantees those are stable and unforgeable, so
// one user cannot exhaust the platform for everyone (the blog's batch
// job lesson).
type RateLimiter struct {
	limit rate.Limit
	burst int

	mu       sync.Mutex
	visitors map[string]*visitor
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(limit rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{limit: limit, burst: burst, visitors: map[string]*visitor{}}
}

func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.visitors[key]
	if !ok {
		if len(l.visitors) >= 4096 {
			l.prune()
		}
		v = &visitor{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.visitors[key] = v
	}
	v.lastSeen = time.Now()
	return v.limiter.Allow()
}

// prune drops visitors idle long enough for their bucket to have fully
// refilled. Called with the mutex held.
func (l *RateLimiter) prune() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for key, v := range l.visitors {
		if v.lastSeen.Before(cutoff) {
			delete(l.visitors, key)
		}
	}
}

// limited wraps a handler with a rate limiter keyed by the caller's
// peer IP.
func limited(l *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			httpError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next(w, r)
	}
}
