package main

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxVisitors caps the number of tracked token buckets so a flood of
// distinct keys cannot grow the map without bound.
const maxVisitors = 4096

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
		if len(l.visitors) >= maxVisitors {
			l.prune()
			// prune only drops idle entries; under a flood of distinct
			// keys within the idle window it may free nothing, so evict
			// the oldest entry to keep the map below the cap.
			for len(l.visitors) >= maxVisitors {
				l.evictOldest()
			}
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

// evictOldest removes the visitor with the oldest lastSeen so a new key
// can be admitted without exceeding the cap. Called with the mutex held.
func (l *RateLimiter) evictOldest() {
	var oldestKey string
	var oldestSeen time.Time
	for key, v := range l.visitors {
		if oldestKey == "" || v.lastSeen.Before(oldestSeen) {
			oldestKey = key
			oldestSeen = v.lastSeen
		}
	}
	if oldestKey != "" {
		delete(l.visitors, oldestKey)
	}
}
