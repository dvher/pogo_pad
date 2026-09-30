package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per client address: each address gets burst
// attempts, refilled evenly over per.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	rate    float64 // tokens per second
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, per time.Duration) *limiter {
	return &limiter{
		burst:   float64(burst),
		rate:    float64(burst) / per.Seconds(),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// allow takes a token for key and reports whether one was available.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) > 10000 {
		// Forget addresses whose buckets have refilled; they are
		// indistinguishable from new ones.
		for k, b := range l.buckets {
			if now.Sub(b.last).Seconds()*l.rate+b.tokens >= l.burst {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientAddr returns the address rate limits apply to.
func (s *Server) clientAddr(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The last entry was added by our proxy; earlier ones are
			// whatever the client claimed.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttle reports whether r may make a password attempt, writing a 429 if not.
func (s *Server) throttle(w http.ResponseWriter, r *http.Request) bool {
	if s.limiter.allow(s.clientAddr(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, "too many attempts; try again in a minute")
	return false
}
