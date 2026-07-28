package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type RateLimiter struct {
	mu             sync.Mutex
	window         time.Duration
	maximum        int
	now            func() time.Time
	entries        map[string]rateEntry
	trustedProxies []*net.IPNet
}

type rateEntry struct {
	started time.Time
	count   int
}

// RateLimiterOption configures optional behavior on a RateLimiter at construction time.
type RateLimiterOption func(*RateLimiter)

// WithTrustedProxies restricts X-Forwarded-For trust to peers whose RemoteAddr falls
// within one of the given networks. Requests from untrusted peers always key on
// RemoteAddr, regardless of any X-Forwarded-For header they present.
func WithTrustedProxies(trustedProxies []*net.IPNet) RateLimiterOption {
	return func(l *RateLimiter) {
		l.trustedProxies = trustedProxies
	}
}

func NewRateLimiter(window time.Duration, maximum int, opts ...RateLimiterOption) *RateLimiter {
	if window <= 0 || maximum <= 0 {
		panic("rate limiter window and maximum must be positive")
	}
	l := &RateLimiter{
		window:  window,
		maximum: maximum,
		now:     time.Now,
		entries: make(map[string]rateEntry),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

func (l *RateLimiter) Allow(key string) bool {
	if key == "" {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictExpiredLocked(now)
	entry, ok := l.entries[key]
	if !ok {
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

// evictExpiredLocked drops entries whose window has elapsed so the map does not grow
// without bound for the lifetime of the process. Must be called with mu held.
func (l *RateLimiter) evictExpiredLocked(now time.Time) {
	for key, entry := range l.entries {
		if now.Sub(entry.started) >= l.window {
			delete(l.entries, key)
		}
	}
}

func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return l.middleware(l.clientIP, next)
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

// clientIP resolves the request's client IP. When the immediate peer (RemoteAddr) is a
// trusted proxy, the left-most address in X-Forwarded-For is used instead, since that is
// the address the proxy itself received the connection from. An untrusted peer's
// X-Forwarded-For header is never honored — a client cannot spoof its own rate-limit key.
func (l *RateLimiter) clientIP(r *http.Request) string {
	remote := requestIP(r)
	if len(l.trustedProxies) == 0 || !ipTrusted(remote, l.trustedProxies) {
		return remote
	}
	forwardedFor := r.Header.Get("X-Forwarded-For")
	if forwardedFor == "" {
		return remote
	}
	first := strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	if first == "" {
		return remote
	}
	return first
}

func ipTrusted(address string, trustedProxies []*net.IPNet) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	for _, network := range trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
