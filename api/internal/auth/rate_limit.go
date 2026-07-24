package auth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type RateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	maximum int
	now     func() time.Time
	entries map[string]rateEntry
}

type rateEntry struct {
	started time.Time
	count   int
}

func NewRateLimiter(window time.Duration, maximum int) *RateLimiter {
	if window <= 0 || maximum <= 0 {
		panic("rate limiter window and maximum must be positive")
	}
	return &RateLimiter{
		window:  window,
		maximum: maximum,
		now:     time.Now,
		entries: make(map[string]rateEntry),
	}
}

func (l *RateLimiter) Allow(key string) bool {
	if key == "" {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || now.Sub(entry.started) >= l.window {
		l.entries[key] = rateEntry{started: now, count: 1}
		return true
	}
	if entry.count >= l.maximum {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return l.middleware(func(r *http.Request) string { return requestIP(r) }, next)
}

func (l *RateLimiter) SessionMiddleware(next http.Handler) http.Handler {
	return l.middleware(func(r *http.Request) string {
		token, _ := SessionToken(r.Context())
		return token
	}, next)
}

func (l *RateLimiter) middleware(key func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(key(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
