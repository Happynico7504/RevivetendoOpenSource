package main

import (
	"net"
	"sync"
	"time"
)

// limiter is a token bucket per source prefix (/24 for IPv4, /56 for IPv6),
// applied to UDP only. It exists to blunt reflection abuse: a spoofed source
// can't make us send it more than a trickle.
type limiter struct {
	qps, burst float64
	mu         sync.Mutex
	buckets    map[string]*bucket
	last       time.Time
	now        func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(qps, burst float64) *limiter {
	return &limiter{qps: qps, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

func prefixKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return string(v4.Mask(net.CIDRMask(24, 32)))
	}
	return string(ip.Mask(net.CIDRMask(56, 128)))
}

func (l *limiter) allow(ip net.IP) bool {
	now := l.now()
	key := prefixKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute { // occasional sweep of idle buckets
		for k, b := range l.buckets {
			if now.Sub(b.at) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * l.qps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
