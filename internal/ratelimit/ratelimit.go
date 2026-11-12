// Package ratelimit is an in-process token-bucket limiter keyed by caller.
// It is sufficient for a single API instance; a multi-instance deployment
// should move buckets to Redis or the edge proxy (see docs).
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
}

func New(perMinute, burst int) *Limiter {
	l := &Limiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}}
	go l.gc()
	return l
}

func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) gc() {
	for range time.Tick(5 * time.Minute) {
		l.mu.Lock()
		for k, b := range l.buckets {
			if time.Since(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}
