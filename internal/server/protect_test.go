package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/perryprojects/siteinfo/internal/brand"
	"github.com/perryprojects/siteinfo/internal/imprint"
	"github.com/perryprojects/siteinfo/internal/observability"
)

const lookupURL = "/v1/imprint?url=acme.de"

func doFrom(t *testing.T, s *Server, remote, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	for _, path := range []string{"/healthz", lookupURL, "/v1/imprint?url=localhost", "/nope"} {
		t.Run(path, func(t *testing.T) {
			h := doFrom(t, s, "203.0.113.7:1", path, nil).Header()
			for k, want := range map[string]string{
				"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
				"Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
			} {
				if got := h.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestAnonymizeIP(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.77":               "203.0.113.0",
		"::ffff:203.0.113.77":        "203.0.113.0",
		"2001:db8:1234:5678:9abc::1": "2001:db8:1234::",
		"not-an-ip":                  "invalid",
	} {
		t.Run(in, func(t *testing.T) {
			if got := anonymizeIP(in); got != want {
				t.Errorf("anonymizeIP(%q) = %q, want %q", in, got, want)
			}
		})
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
	obs := &recObserver{}
	s := newTestServer(t, &fakeLoader{}, l, WithMaxInflight(1), WithObserver(obs), WithRetryAfter(3*time.Second))

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
