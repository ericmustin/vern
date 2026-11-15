package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runFrom(t *testing.T, s *server, addr string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := postRaw(readDemo(t))
	req.RemoteAddr = addr
	for k, v := range hdr {
		req.Header[k] = v
	}
	return serve(s, req)
}

func TestRateLimit_PerIPBurstThen429(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil)})
	for i := 0; i < 3; i++ {
		if rec := runFrom(t, s, "198.51.100.1:5000", nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d %s, want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := runFrom(t, s, "198.51.100.1:5001", nil) // same IP, new port
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", rec.Code)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n <= 0 {
		t.Fatalf("Retry-After = %q, want > 0", rec.Header().Get("Retry-After"))
	}
	if rec := runFrom(t, s, "198.51.100.2:5000", nil); rec.Code != http.StatusOK {
		t.Fatalf("second IP = %d, want 200", rec.Code)
	}
}

func TestRateLimit_RefillsAndNeverTakesTheSlot(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil), RatePerHour: 6, RateBurst: 1})
	s.now = func() time.Time { return now }
	if rec := runFrom(t, s, "198.51.100.1:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("first = %d", rec.Code)
	}
	if rec := runFrom(t, s, "198.51.100.1:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", rec.Code)
	}
	if len(s.sem) != 0 {
		t.Fatal("a limited request holds the run slot")
	}
	now = now.Add(10 * time.Minute) // 6/hour = one token per 10 minutes
	if rec := runFrom(t, s, "198.51.100.1:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("after refill = %d, want 200", rec.Code)
	}
}

func TestRateLimit_EvictsIdleBuckets(t *testing.T) {
	l := newIPLimiter(6, 3)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		l.allow("10.0.0."+strconv.Itoa(i), now)
	}
	if len(l.buckets) != 50 {
		t.Fatalf("buckets = %d", len(l.buckets))
	}
	// Past the refill time (3 tokens at 6/hour = 30 min) and the sweep period.
	l.allow("203.0.113.1", now.Add(time.Hour))
	if len(l.buckets) != 1 {
		t.Fatalf("buckets after sweep = %d, want 1", len(l.buckets))
	}
}

func TestRateLimit_XFFTrustedOnlyFromProxy(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name, remote string
		xff          []string
		trusted      []netip.Prefix
		want         string
	}{
		{"via trusted proxy", "10.1.2.3:4000", []string{"203.0.113.9, 10.1.2.3"}, trusted, "203.0.113.9"},
		{"from untrusted peer", "198.51.100.7:4000", []string{"203.0.113.9, 10.1.2.3"}, trusted, "198.51.100.7"},
		{"forged left entries ignored", "10.1.2.3:4000", []string{"1.1.1.1, 203.0.113.9, 10.9.9.9"}, trusted, "203.0.113.9"},
		{"several header lines", "10.1.2.3:4000", []string{"1.1.1.1", "203.0.113.9"}, trusted, "203.0.113.9"},
		{"all hops trusted", "10.1.2.3:4000", []string{"10.5.5.5"}, trusted, "10.1.2.3"},
		{"no header", "10.1.2.3:4000", nil, trusted, "10.1.2.3"},
		{"no trusted proxies", "10.1.2.3:4000", []string{"203.0.113.9"}, nil, "10.1.2.3"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		req.RemoteAddr = c.remote
		req.Header["X-Forwarded-For"] = c.xff
		if got := clientIP(req, c.trusted); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}

	// End to end: two clients behind the same proxy are limited separately.
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil), RateBurst: 1, TrustedProxies: trusted})
	hdr := func(ip string) http.Header { return http.Header{"X-Forwarded-For": {ip}} }
	if rec := runFrom(t, s, "10.1.2.3:1", hdr("203.0.113.9")); rec.Code != http.StatusOK {
		t.Fatalf("client A = %d", rec.Code)
	}
	if rec := runFrom(t, s, "10.1.2.3:1", hdr("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A again = %d, want 429", rec.Code)
	}
	if rec := runFrom(t, s, "10.1.2.3:1", hdr("203.0.113.10")); rec.Code != http.StatusOK {
		t.Fatalf("client B = %d, want 200", rec.Code)
	}
}

func TestDailyIngestCap(t *testing.T) {
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	s := newTestServer(t, Config{
		Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil),
		RatePerHour: 100000, RateBurst: 100000,
		DailyIngestBytes: 1000, // the demo sample is far larger decompressed
	})
	s.now = func() time.Time { return now }

	// The upload that crosses the cap still completes.
	if rec := runFrom(t, s, "198.51.100.1:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("crossing upload = %d %s, want 200", rec.Code, rec.Body.String())
	}
	rec := runFrom(t, s, "198.51.100.2:1", nil)
	if rec.Code != http.StatusTooManyRequests || errorBody(t, rec) != "daily capacity reached" {
		t.Fatalf("next upload = %d %s, want 429 daily capacity reached", rec.Code, rec.Body.String())
	}
	if n, _ := strconv.Atoi(rec.Header().Get("Retry-After")); n != 3600 {
		t.Fatalf("Retry-After = %q, want 3600 (one hour to UTC midnight)", rec.Header().Get("Retry-After"))
	}
	if len(s.sem) != 0 {
		t.Fatal("a capped request holds the run slot")
	}

	now = now.Add(time.Hour) // UTC midnight
	if rec := runFrom(t, s, "198.51.100.2:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("after the day boundary = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestDailyIngestCap_FailedDecodeDoesNotCount(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil), DailyIngestBytes: 10})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"hello":"world"}`))
	if rec := serve(s, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad upload = %d", rec.Code)
	}
	if rec := runFrom(t, s, "198.51.100.1:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("after a rejected upload = %d, want 200", rec.Code)
	}
}

func TestClientIP_IPv6AndGarbage(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	key := func(remote string, xff string, tr []netip.Prefix) string {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return clientIP(req, tr)
	}
	a := key("[2001:db8:1:2:aaaa::1]:1", "", nil)
	b := key("[2001:db8:1:2:bbbb::9]:1", "", nil)
	c := key("[2001:db8:1:3::1]:1", "", nil)
	if a != b {
		t.Errorf("same /64 keyed differently: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different /64s share a key: %q", a)
	}
	if got := key("[::ffff:198.51.100.7]:1", "", nil); got != "198.51.100.7" {
		t.Errorf("mapped IPv4 keyed %q", got)
	}
	if got := key("10.1.2.3:1", "2001:db8:1:2:cccc::5", trusted); got != a {
		t.Errorf("XFF IPv6 keyed %q, want %q", got, a)
	}
	// An unparseable hop falls back to the peer, never the raw string.
	if got := key("10.1.2.3:1", "203.0.113.9, not-an-ip", trusted); got != "10.1.2.3" {
		t.Errorf("garbled hop keyed %q, want the peer", got)
	}
}

func TestRateLimit_BucketCapRefusesNewKeysKeepsLiveOnes(t *testing.T) {
	l := newIPLimiter(6, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxBuckets; i++ {
		l.buckets["k"+strconv.Itoa(i)] = &bucket{tokens: 0, at: now} // drained and live
	}
	l.lastSweep = now
	ok, wait := l.allow("newcomer", now)
	if ok || wait <= 0 {
		t.Fatalf("newcomer at the cap = %v, %v; want refused with a wait", ok, wait)
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("buckets = %d, live ones were evicted or one was added", len(l.buckets))
	}
	// Once the buckets have refilled, the sweep frees room.
	if ok, _ := l.allow("newcomer", now.Add(2*time.Hour)); !ok {
		t.Fatal("newcomer refused after the buckets refilled")
	}
}
