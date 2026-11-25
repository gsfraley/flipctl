package api

import (
	"math"
	"sync"
	"time"
)

// rateLimiter is a token bucket per plugin id. It bounds how often any client
// (a person, a script or an assistant) can start work on behalf of one plugin.
type rateLimiter struct {
	mu      sync.Mutex
	perSec  float64
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute float64, burst int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{perSec: perMinute / 60, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// allow consumes one token for key. When none is available it reports how
// long until one will be.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
}
