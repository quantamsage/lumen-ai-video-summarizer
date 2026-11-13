package web

import (
	"math"
	"sync"
	"time"
)

// rateRule allows Burst requests at once, refilling at Rate per second.
type rateRule struct {
	Rate  float64
	Burst float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is an in-memory token-bucket limiter keyed by arbitrary strings.
type rateLimiter struct {
	mu      sync.Mutex
	rules   map[string]rateRule
	buckets map[string]*bucket
	now     func() time.Time
}

func newRateLimiter(rules map[string]rateRule) *rateLimiter {
	return &rateLimiter{rules: rules, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow consumes a token for key under the named rule. When denied it
// returns how long until a token is available.
func (l *rateLimiter) Allow(rule, key string) (bool, time.Duration) {
	r, ok := l.rules[rule]
	if !ok {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: r.Burst, last: t}
		l.buckets[key] = b
	}
	b.tokens = math.Min(r.Burst, b.tokens+t.Sub(b.last).Seconds()*r.Rate)
	b.last = t
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / r.Rate * float64(time.Second))
	return false, wait
}

// Sweep drops buckets that have been idle long enough to be full again.
func (l *rateLimiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	for k, b := range l.buckets {
		if t.Sub(b.last) > time.Hour {
			delete(l.buckets, k)
		}
	}
}

func defaultRateRules() map[string]rateRule {
	perMinute := func(n float64) float64 { return n / 60 }
	perHour := func(n float64) float64 { return n / 3600 }
	return map[string]rateRule{
		"login":    {Rate: perMinute(5), Burst: 10},
		"register": {Rate: perHour(10), Burst: 5},
		"reset":    {Rate: perHour(5), Burst: 3},
		"upload":   {Rate: perHour(30), Burst: 10},
		"generate": {Rate: perHour(30), Burst: 10},
		"submit":   {Rate: perMinute(10), Burst: 20},
		"account":  {Rate: perMinute(5), Burst: 10},
	}
}
