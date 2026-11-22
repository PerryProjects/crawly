package server

import (
	"math"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// keyedLimiter is a set of token buckets addressed by key (client IP, token
// index). Memory is bounded: idle buckets are swept, and when the table is
// full all unknown keys share one overflow bucket. That makes a botnet or an
// IPv6 address-rotating client degrade into a shared, tighter limit instead of
// exhausting memory.
type keyedLimiter struct {
	mu        sync.Mutex
	m         map[string]*limiterEntry
	limit     rate.Limit
	burst     int
	maxKeys   int
	idleTTL   time.Duration
	overflow  *rate.Limiter
	nextSweep time.Time
}

type limiterEntry struct {
	lim  *rate.Limiter
	last time.Time
}

// newKeyedLimiter returns nil when perSecond <= 0 (limiting disabled).
func newKeyedLimiter(perSecond float64, burst, maxKeys int) *keyedLimiter {
	if perSecond <= 0 {
		return nil
	}
	burst = max(burst, 1)
	// A bucket idle for a full refill period is indistinguishable from a fresh one.
	refill := time.Duration(float64(burst) / perSecond * float64(time.Second))
	return &keyedLimiter{
		m:        make(map[string]*limiterEntry),
		limit:    rate.Limit(perSecond),
		burst:    burst,
		maxKeys:  maxKeys,
		idleTTL:  max(refill, time.Minute),
		overflow: rate.NewLimiter(rate.Limit(perSecond*100), burst*100),
	}
}

// take consumes one token for key. When denied it reports how long until a token is available.
func (l *keyedLimiter) take(key string, now time.Time) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.bucket(key, now).ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now) // we do not queue, we reject
		return false, d
	}
	return true, 0
}

// check reports whether key has a token left without consuming one.
func (l *keyedLimiter) check(key string, now time.Time) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, found := l.m[key]
	if !found {
		return true, 0
	}
	tokens := e.lim.TokensAt(now)
	if tokens >= 1 {
		return true, 0
	}
	return false, time.Duration((1 - tokens) / float64(l.limit) * float64(time.Second))
}

func (l *keyedLimiter) bucket(key string, now time.Time) *rate.Limiter {
	if e, ok := l.m[key]; ok {
		e.last = now
		return e.lim
	}
	if now.After(l.nextSweep) {
		for k, e := range l.m {
			if now.Sub(e.last) > l.idleTTL {
				delete(l.m, k)
			}
		}
		l.nextSweep = now.Add(l.idleTTL / 4)
	}
	if len(l.m) >= l.maxKeys {
		return l.overflow
	}
	e := &limiterEntry{lim: rate.NewLimiter(l.limit, l.burst), last: now}
	l.m[key] = e
	return e.lim
}

func (l *keyedLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// clientKey reduces an IP string to a limiter key. IPv6 addresses are grouped
// by /64, the smallest block a single subscriber typically controls, so rotating
// through addresses of one network does not multiply the budget.
func clientKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// retryAfterSeconds renders a wait as a whole-second Retry-After value (at least 1).
func retryAfterSeconds(d time.Duration) int {
	return max(int(math.Ceil(d.Seconds())), 1)
}
