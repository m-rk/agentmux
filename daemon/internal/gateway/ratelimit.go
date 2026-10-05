package gateway

import (
	"sync"
	"time"
)

// Limit is a token bucket: Burst requests at once, refilled at PerMinute.
// PerMinute <= 0 disables the limit.
type Limit struct {
	PerMinute float64
	Burst     int
}

// Default limits per principal.
var (
	DefaultSendLimit  = Limit{PerMinute: 30, Burst: 10}
	DefaultOtherLimit = Limit{PerMinute: 300, Burst: 60}
)

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter keeps one bucket per principal.
type limiter struct {
	limit Limit
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

func newLimiter(l Limit, now func() time.Time) *limiter {
	return &limiter{limit: l, now: now, buckets: map[string]*bucket{}}
}

// allow takes one token for principal, reporting whether there was one.
func (l *limiter) allow(principal string) bool {
	if l.limit.PerMinute <= 0 {
		return true
	}
	burst := float64(max(l.limit.Burst, 1))
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[principal]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets[principal] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Minutes()*l.limit.PerMinute)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
