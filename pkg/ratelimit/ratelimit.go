// Package ratelimit provides an in-memory per-client token bucket limiter
// used to protect the GraphQL endpoint against abusive query volume.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter holds one token bucket per client key. It is safe for concurrent
// use. Stale buckets are lazily evicted, so memory usage stays bounded by the
// number of distinct active clients instead of the total clients ever seen.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64
	burst     float64
	lastSweep time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a Limiter that refills at rate tokens per second and allows
// short bursts of up to burst requests.
func New(rate float64, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   float64(burst),
	}
}

// Allow reports whether a request arriving now for key is permitted.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst}
		l.buckets[key] = b
	}

	// Refill based on elapsed time since the bucket was last touched.
	b.tokens += now.Sub(b.lastSeen).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}

	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}

// sweepLocked drops buckets that have been idle for 10 minutes. It runs at
// most once per minute.
func (l *Limiter) sweepLocked(now time.Time) {
	if l.lastSweep.IsZero() {
		l.lastSweep = now
		return
	}

	if now.Sub(l.lastSweep) < time.Minute {
		return
	}

	l.lastSweep = now

	idleSince := now.Add(-10 * time.Minute)

	for key, b := range l.buckets {
		if b.lastSeen.Before(idleSince) {
			delete(l.buckets, key)
		}
	}
}

// ClientIP extracts the client IP used as the rate limit key. X-Forwarded-For
// is only trusted when trustProxy is enabled (i.e. Hetty runs behind a reverse
// proxy that overwrites that header); otherwise the header is client supplied
// and trivially spoofable to evade limiting.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// X-Forwarded-For is a comma separated list; the leftmost entry is
		// the original client.
		for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
			if ip := net.ParseIP(strings.TrimSpace(part)); ip != nil {
				return ip.String()
			}
		}
	}

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}

	return r.RemoteAddr
}
