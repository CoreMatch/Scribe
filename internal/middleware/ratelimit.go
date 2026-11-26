package middleware

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/corematch/scribe/internal/protocol"
)

type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	rate       float64
	burst      float64
	maxEntries int
	now        func() time.Time
	lastClean  time.Time
}

type tokenBucket struct {
	tokens     float64
	lastRefill time.Time
}

func NewRateLimiter(rate, burst float64, maxEntries int) *RateLimiter {
	return &RateLimiter{
		buckets:    make(map[string]*tokenBucket),
		rate:       rate,
		burst:      burst,
		maxEntries: maxEntries,
		now:        time.Now,
		lastClean:  time.Now(),
	}
}

func (l *RateLimiter) Allow(key string) bool {
	if key == "" || l.rate <= 0 || l.burst <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= l.maxEntries {
			l.cleanup(now)
			if len(l.buckets) >= l.maxEntries {
				return false
			}
		}
		bucket = &tokenBucket{tokens: l.burst, lastRefill: now}
		l.buckets[key] = bucket
	}
	elapsed := now.Sub(bucket.lastRefill).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(l.burst, bucket.tokens+elapsed*l.rate)
		bucket.lastRefill = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

func (l *RateLimiter) cleanup(now time.Time) {
	if now.Sub(l.lastClean) < time.Minute {
		return
	}
	cutoff := now.Add(-10 * time.Minute)
	for key, bucket := range l.buckets {
		if bucket.lastRefill.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
	l.lastClean = now
}

func RateLimit(limiter *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow(ClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			protocol.WriteError(w, http.StatusTooManyRequests, "TooManyRequestsException", "Too many requests. Try again later.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RateLimitAuthentication(limiter, signoutLimiter *RateLimiter, next http.Handler) http.Handler {
	limited := RateLimit(limiter, next)
	limitedSignout := RateLimit(signoutLimiter, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authserver/authenticate":
			limited.ServeHTTP(w, r)
		case "/authserver/signout":
			limitedSignout.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
