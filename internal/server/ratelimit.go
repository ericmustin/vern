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

	// maxBuckets bounds the per-IP map; at the cap, new clients are refused
	// rather than live buckets evicted.
	maxBuckets = 100_000

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
		if len(l.buckets) >= maxBuckets {
			l.sweepNow(now)
			if len(l.buckets) >= maxBuckets {
				return false, time.Duration(burstRefill(l) * float64(time.Second))
			}
		}
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
	l.sweepNow(now)
}

// burstRefill is how long a drained bucket takes to hold a token again, the
// wait quoted to clients refused at the cap.
func burstRefill(l *ipLimiter) float64 { return 1 / l.perSec }

func (l *ipLimiter) sweepNow(now time.Time) {
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
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil {
		return peer // not an IP (a test transport): key on it as given
	}
	peerAddr = peerAddr.Unmap()
	if len(trusted) == 0 || !isTrusted(peerAddr, trusted) {
		return limiterKey(peerAddr)
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		a, err := netip.ParseAddr(hop)
		if err != nil {
			break // a garbled hop: the entries to its left cannot be trusted either
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return limiterKey(a)
		}
	}
	return limiterKey(peerAddr)
}

// limiterKey is the bucket key for an address: IPv6 clients are grouped by
// /64, since one subscriber holds a whole /64 and could otherwise mint
// addresses at will.
func limiterKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
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
