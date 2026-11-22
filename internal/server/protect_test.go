package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/OWNER/siteinfo/internal/brand"
	"github.com/OWNER/siteinfo/internal/imprint"
	"github.com/OWNER/siteinfo/internal/observability"
)

func TestKeyedLimiter(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	l := newKeyedLimiter(2, 3, 1000) // 2/s, burst 3
	for i := range 3 {
		if ok, _ := l.take("a", t0); !ok {
			t.Fatalf("burst token %d denied", i)
		}
	}
	ok, wait := l.take("a", t0)
	if ok || wait < 400*time.Millisecond || wait > 600*time.Millisecond {
		t.Fatalf("4th take: ok=%v wait=%s, want denied with ~500ms", ok, wait)
	}
	if ok, _ := l.take("b", t0); !ok {
		t.Fatal("keys must be independent")
	}
	if ok, _ := l.take("a", t0.Add(600*time.Millisecond)); !ok {
		t.Fatal("token must refill")
	}
	// A denied take must not consume or queue: after 1s there are exactly 2 tokens again.
	l2 := newKeyedLimiter(1, 1, 1000)
	l2.take("x", t0)
	for range 10 {
		l2.take("x", t0) // denied attempts
	}
	if ok, _ := l2.take("x", t0.Add(1100*time.Millisecond)); !ok {
		t.Fatal("denied attempts must not push the next free token into the future")
	}
}

func TestKeyedLimiterMemoryIsBounded(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	l := newKeyedLimiter(1, 1, 1000)
	allowed := 0
	for i := range 50_000 { // an attacker cycling through addresses
		if ok, _ := l.take("ip-"+strconv.Itoa(i), t0); ok {
			allowed++
		}
	}
	if l.size() > 1000 {
		t.Fatalf("table grew to %d keys, cap is 1000", l.size())
	}
	// Past the cap, unknown keys share the overflow bucket: far fewer than 50k pass.
	if allowed > 1000+100 {
		t.Fatalf("allowed %d requests from 50k distinct keys", allowed)
	}

	// Idle buckets are swept, freeing room for new keys.
	later := t0.Add(10 * time.Minute)
	if ok, _ := l.take("fresh", later); !ok {
		t.Fatal("fresh key denied after sweep")
	}
	if l.size() > 2 {
		t.Fatalf("idle entries were not swept, size = %d", l.size())
	}
}

func TestNewKeyedLimiterDisabled(t *testing.T) {
	if newKeyedLimiter(0, 10, 1000) != nil {
		t.Fatal("rate 0 must disable the limiter")
	}
}

func TestClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":                      "203.0.113.7",
		"::ffff:203.0.113.7":               "203.0.113.7",
		"2001:db8:1:2:aaaa:bbbb:cccc:dddd": "2001:db8:1:2::/64",
		"2001:db8:1:2:1111:2222:3333:4444": "2001:db8:1:2::/64", // same /64, same key
		"not-an-ip":                        "not-an-ip",
	} {
		if got := clientKey(in); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func protectedServer(t *testing.T, p Protection, l fakeLookups, opts ...Option) (*Server, *recObserver) {
	t.Helper()
	obs := &recObserver{}
	return newTestServer(t, &fakeLoader{}, l, append([]Option{WithProtection(p), WithObserver(obs)}, opts...)...), obs
}

func doFrom(t *testing.T, s *Server, remote, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

const lookupURL = "/v1/imprint?url=acme.de"

func TestIPRateLimit(t *testing.T) {
	s, obs := protectedServer(t, Protection{IPRate: 0.5, IPBurst: 2, MaxTrackedKeys: 1000}, fakeLookups{})
	for i := range 2 {
		if rec := doFrom(t, s, "198.51.100.1:1000", lookupURL, nil); rec.Code != 200 {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	rec := doFrom(t, s, "198.51.100.1:1001", lookupURL, nil)
	if rec.Code != 429 || errCode(t, rec) != "rate_limited" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra < 1 || ra > 3 {
		t.Fatalf("Retry-After = %q, want the time until the next token (about 2s)", rec.Header().Get("Retry-After"))
	}
	if rec := doFrom(t, s, "198.51.100.2:1000", lookupURL, nil); rec.Code != 200 {
		t.Fatalf("another IP must not be affected: %d", rec.Code)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := doFrom(t, s, "198.51.100.1:1000", p, nil); rec.Code != 200 {
			t.Fatalf("%s must never be rate limited: %d", p, rec.Code)
		}
	}
	if len(obs.rejected) != 1 || obs.rejected[0] != "ip_rate" {
		t.Fatalf("rejections = %v", obs.rejected)
	}
}

func TestXForwardedForIsIgnoredWithoutTrustedProxy(t *testing.T) {
	s, _ := protectedServer(t, Protection{IPRate: 0.01, IPBurst: 1, MaxTrackedKeys: 1000}, fakeLookups{})
	if rec := doFrom(t, s, "198.51.100.1:1", lookupURL, map[string]string{"X-Forwarded-For": "1.1.1.1"}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	// Rotating the header must not buy a fresh budget.
	if rec := doFrom(t, s, "198.51.100.1:1", lookupURL, map[string]string{"X-Forwarded-For": "2.2.2.2"}); rec.Code != 429 {
		t.Fatalf("spoofed XFF bypassed the limit: %d", rec.Code)
	}
}

func TestXForwardedForWithTrustedProxy(t *testing.T) {
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	s, _ := protectedServer(t, Protection{IPRate: 0.01, IPBurst: 1, MaxTrackedKeys: 1000}, fakeLookups{}, WithTrustedProxies([]*net.IPNet{proxyNet}))

	// Behind the trusted proxy, clients are told apart by their forwarded address.
	for _, client := range []string{"203.0.113.1", "203.0.113.2"} {
		if rec := doFrom(t, s, "10.1.2.3:4000", lookupURL, map[string]string{"X-Forwarded-For": client}); rec.Code != 200 {
			t.Fatalf("client %s = %d", client, rec.Code)
		}
	}
	if rec := doFrom(t, s, "10.1.2.3:4000", lookupURL, map[string]string{"X-Forwarded-For": "203.0.113.1"}); rec.Code != 429 {
		t.Fatalf("same forwarded client must be limited: %d", rec.Code)
	}
	// A client that is not a trusted proxy cannot forge the header.
	if rec := doFrom(t, s, "198.51.100.9:4000", lookupURL, map[string]string{"X-Forwarded-For": "203.0.113.50"}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := doFrom(t, s, "198.51.100.9:4001", lookupURL, map[string]string{"X-Forwarded-For": "203.0.113.51"}); rec.Code != 429 {
		t.Fatalf("untrusted peer forged XFF: %d", rec.Code)
	}
}

func TestIPv6AddressRotationSharesOneBudget(t *testing.T) {
	s, _ := protectedServer(t, Protection{IPRate: 0.01, IPBurst: 1, MaxTrackedKeys: 1000}, fakeLookups{})
	if rec := doFrom(t, s, "[2001:db8:1:2:aaaa::1]:80", lookupURL, nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := doFrom(t, s, "[2001:db8:1:2:bbbb::9]:80", lookupURL, nil); rec.Code != 429 {
		t.Fatalf("a second address in the same /64 got its own budget: %d", rec.Code)
	}
}

func TestAuthFailureBlocking(t *testing.T) {
	s, obs := protectedServer(t, Protection{AuthFailuresPerMinute: 3, MaxTrackedKeys: 1000}, fakeLookups{})
	bad := map[string]string{"Authorization": "Bearer wrong"}
	for i := range 3 {
		if rec := doFrom(t, s, "198.51.100.1:1", lookupURL, bad); rec.Code != 401 {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	// Further failures from that IP are throttled ...
	rec := doFrom(t, s, "198.51.100.1:1", lookupURL, bad)
	if rec.Code != 429 || errCode(t, rec) != "too_many_auth_failures" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	// ... but a valid token from the same IP still works: behind a proxy all callers share that IP,
	// and an attacker must not be able to lock everyone out.
	if rec := doFrom(t, s, "198.51.100.1:1", lookupURL, nil); rec.Code != 200 {
		t.Fatalf("valid token was locked out: %d", rec.Code)
	}
	if rec := doFrom(t, s, "198.51.100.2:1", lookupURL, bad); rec.Code != 401 {
		t.Fatalf("other IPs have their own budget: %d", rec.Code)
	}
	found := false
	for _, r := range obs.rejected {
		found = found || r == "auth_failures"
	}
	if !found {
		t.Fatalf("rejections = %v", obs.rejected)
	}
}

func TestSuccessfulRequestsDoNotConsumeAuthFailureBudget(t *testing.T) {
	s, _ := protectedServer(t, Protection{AuthFailuresPerMinute: 2, MaxTrackedKeys: 1000}, fakeLookups{})
	for range 20 {
		if rec := doFrom(t, s, "198.51.100.1:1", lookupURL, nil); rec.Code != 200 {
			t.Fatalf("legitimate traffic was blocked: %d", rec.Code)
		}
	}
}

func TestTokenRateLimit(t *testing.T) {
	l := fakeLookups{}
	obs := &recObserver{}
	l.imprintFn = func(context.Context, string) (*imprint.Imprint, error) { return sampleImprint(), nil }
	l.brandFn = func(context.Context, string) (*brand.Brand, error) { return &brand.Brand{}, nil }
	s, err := New(&fakeLoader{}, l.Imprint(), l.Brand(), pingFunc(okPing), []string{"first-token-0123456789", "second-token-0123456789"},
		WithProtection(Protection{TokenRate: 0.01, TokenBurst: 1, MaxTrackedKeys: 1000}), WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	as := func(tk string) int {
		return doFrom(t, s, "198.51.100.1:1", lookupURL, map[string]string{"Authorization": "Bearer " + tk}).Code
	}
	if as("first-token-0123456789") != 200 {
		t.Fatal("first request")
	}
	rec := doFrom(t, s, "198.51.100.1:1", lookupURL, map[string]string{"Authorization": "Bearer first-token-0123456789"})
	if rec.Code != 429 || errCode(t, rec) != "token_rate_limited" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if as("second-token-0123456789") != 200 {
		t.Fatal("each token has its own budget")
	}
}

func TestInflightLimitShedsLoad(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	l := fakeLookups{imprintFn: func(context.Context, string) (*imprint.Imprint, error) {
		started <- struct{}{}
		<-release
		return sampleImprint(), nil
	}}
	s, obs := protectedServer(t, Protection{MaxInflight: 1, MaxTrackedKeys: 1000}, l, WithRetryAfter(3*time.Second))

	first := make(chan int, 1)
	go func() { first <- doFrom(t, s, "198.51.100.1:1", lookupURL, nil).Code }()
	<-started

	rec := doFrom(t, s, "198.51.100.2:1", "/v1/imprint?url=other.de", nil)
	if rec.Code != 503 || errCode(t, rec) != "overloaded" || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := doFrom(t, s, "198.51.100.2:1", "/healthz", nil); rec.Code != 200 {
		t.Fatalf("probes must not be shed: %d", rec.Code)
	}
	close(release)
	if code := <-first; code != 200 {
		t.Fatalf("in-flight request = %d", code)
	}
	if rec := doFrom(t, s, "198.51.100.2:1", lookupURL, nil); rec.Code != 200 {
		t.Fatalf("slot must be released: %d", rec.Code)
	}
	if len(obs.rejected) != 1 || obs.rejected[0] != "inflight" {
		t.Fatalf("rejections = %v", obs.rejected)
	}
}

func BenchmarkAccessLog(b *testing.B) {
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel)
	l := fakeLookups{}
	l.imprintFn = func(context.Context, string) (*imprint.Imprint, error) { return nil, nil }
	l.brandFn = func(context.Context, string) (*brand.Brand, error) { return nil, nil }
	s, _ := New(&fakeLoader{}, l.Imprint(), l.Brand(), pingFunc(okPing), nil, WithoutAuth(), WithLogger(zap.New(core)))
	ctx := observability.WithRequestFields(context.Background(), &observability.RequestFields{RequestID: "abc123", Host: "acme.de", Kind: "imprint"})
	e := &accessEntry{Method: "GET", Route: "/v1/imprint", RemoteIP: "203.0.113.7", Cache: "HIT", Status: 200, Bytes: 4321, Duration: 12 * time.Millisecond}
	b.ReportAllocs()
	for b.Loop() {
		s.logAccess(ctx, e)
	}
}
