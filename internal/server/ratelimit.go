package server

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	defaultRatePerHour      = 6
	defaultRateBurst        = 3
	defaultDailyIngestBytes = 2 << 30

	// sweepEvery is how often (on access) idle buckets are evicted.
	sweepEvery = 10 * time.Minute
)

// ipLimiter is a token bucket per client IP. Idle buckets are evicted from
// allow, so memory is bounded by the IPs seen within a refill period.
type ipLimiter struct {
	mu        sync.Mutex
	perSec    float64
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newIPLimiter(perHour, burst int) *ipLimiter {
	return &ipLimiter{
		perSec:  float64(perHour) / 3600,
		burst:   float64(burst),
		buckets: map[string]*bucket{},
	}
}

// allow takes one token for key. When none is left it reports how long until
// the next one.
func (l *ipLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.perSec)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled completely: they are
// indistinguishable from a new client's.
func (l *ipLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// clientIP is the address a request is rate-limited under: the peer, unless
// the peer is a trusted proxy, in which case X-Forwarded-For is read right to
// left and the first address that is not a trusted proxy wins. With no
// trusted proxies the header is ignored, since any client can forge it.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !isTrusted(hop, trusted) {
			return hop
		}
	}
	return peer
}

func isTrusted(addr string, trusted []netip.Prefix) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// dailyCap counts decompressed upload bytes per UTC day, in memory only: a
// restart resets it. The check runs before an upload and the bytes are added
// after it decodes, so the upload that crosses the cap finishes and the next
// one is refused.
type dailyCap struct {
	mu  sync.Mutex
	max int64
	day time.Time // start of the UTC day n belongs to
	n   int64
}

func (c *dailyCap) roll(now time.Time) {
	if d := now.UTC().Truncate(24 * time.Hour); !d.Equal(c.day) {
		c.day, c.n = d, 0
	}
}

// full reports whether today's budget is spent, and when it resets.
func (c *dailyCap) full(now time.Time) (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roll(now)
	return c.n >= c.max, c.day.Add(24 * time.Hour).Sub(now)
}

func (c *dailyCap) add(now time.Time, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roll(now)
	c.n += n
}
