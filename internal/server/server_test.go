package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/perryprojects/siteinfo/internal/brand"
	"github.com/perryprojects/siteinfo/internal/cache"
	"github.com/perryprojects/siteinfo/internal/crawler"
	"github.com/perryprojects/siteinfo/internal/imprint"
	"github.com/perryprojects/siteinfo/internal/urlnorm"
)

const token = "test-token-0123456789"

// fakeLoader runs fetch inline and returns scripted results.
type fakeLoader struct {
	mu    sync.Mutex
	opts  []cache.LoadOptions
	keys  []cache.Key
	entry *cache.Entry // when set, returned instead of calling fetch
	err   error        // when set, returned instead of calling fetch
	block bool         // wait for ctx.Done()
}

func (f *fakeLoader) Load(ctx context.Context, key cache.Key, fetch cache.FetchFunc, opt cache.LoadOptions) (cache.Entry, error) {
	f.mu.Lock()
	f.opts, f.keys = append(f.opts, opt), append(f.keys, key)
	entry, err, block := f.entry, f.err, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return cache.Entry{}, ctx.Err()
	}
	if err != nil {
		return cache.Entry{}, err
	}
	if entry != nil {
		return *entry, nil
	}
	fetched, ferr := fetch(ctx)
	if ferr != nil {
		return cache.Entry{}, ferr
	}
	return cache.Entry{Envelope: cache.Envelope{SchemaVersion: 1, Status: cache.StatusOK, FetchedAt: time.Unix(1700000000, 0).UTC(), Data: fetched.Data, Partial: fetched.Partial}}, nil
}

type fakeLookups struct {
	imprintFn func(context.Context, string) (*imprint.Imprint, error)
	brandFn   func(context.Context, string) (*brand.Brand, error)
}

func (f fakeLookups) Imprint() ImprintLookup { return imprintFunc(f.imprintFn) }
func (f fakeLookups) Brand() BrandLookup     { return brandFunc(f.brandFn) }

type imprintFunc func(context.Context, string) (*imprint.Imprint, error)

func (f imprintFunc) Lookup(ctx context.Context, h string) (*imprint.Imprint, error) {
	return f(ctx, h)
}

type brandFunc func(context.Context, string) (*brand.Brand, error)

func (f brandFunc) Lookup(ctx context.Context, h string) (*brand.Brand, error) { return f(ctx, h) }

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func okPing(context.Context) error { return nil }

func strp(s string) *string { return &s }

func sampleImprint() *imprint.Imprint {
	return &imprint.Imprint{
		ImprintURL: "https://acme.de/impressum",
		Fields:     imprint.Fields{CompanyName: strp("ACME GmbH"), RepresentedBy: []string{"Max Mustermann"}, Country: strp("DE")},
		Text:       "# Impressum\nACME GmbH",
		Warnings:   []imprint.Warning{},
	}
}

func newTestServer(t *testing.T, loader cache.Loader, l fakeLookups, opts ...Option) *Server {
	t.Helper()
	if l.imprintFn == nil {
		l.imprintFn = func(context.Context, string) (*imprint.Imprint, error) { return sampleImprint(), nil }
	}
	if l.brandFn == nil {
		l.brandFn = func(context.Context, string) (*brand.Brand, error) {
			return &brand.Brand{Description: strp("hi"), Fonts: []brand.Font{}, ToneOfVoice: []string{}}, nil
		}
	}
	s, err := New(loader, l.Imprint(), l.Brand(), pingFunc(okPing), []string{token}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(t *testing.T, s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	if _, ok := hdr["Authorization"]; !ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	e, _ := decodeBody(t, rec)["error"].(map[string]any)
	if e == nil || e["message"] == "" {
		t.Fatalf("not an error body: %s", rec.Body.String())
	}
	return e["code"].(string)
}

func TestImprintSuccess(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=https://www.ACME.de/some/path?utm_source=x", nil)
	if rec.Code != 200 || rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
	body := decodeBody(t, rec)
	if body["host"] != "acme.de" || body["cached"] != false || body["company_name"] != "ACME GmbH" ||
		body["imprint_url"] != "https://acme.de/impressum" || body["text"] != "# Impressum\nACME GmbH" || body["fetched_at"] == nil {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["warnings"].([]any); !ok {
		t.Errorf("warnings must be an array: %v", body["warnings"])
	}
	if body["vat_id"] != nil {
		t.Errorf("vat_id must be present as null: %v", body)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("request id header missing")
	}
}

func TestBrandSuccessAndKey(t *testing.T) {
	fl := &fakeLoader{}
	s := newTestServer(t, fl, fakeLookups{})
	rec := do(t, s, http.MethodGet, "/v1/brand?url=acme.de", nil)
	if rec.Code != 200 || decodeBody(t, rec)["description"] != "hi" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if fl.keys[0] != (cache.Key{Kind: cache.KindBrand, Host: "acme.de"}) {
		t.Fatalf("key = %+v", fl.keys[0])
	}
}

func TestCachedFlagAndHitHeader(t *testing.T) {
	data, _ := json.Marshal(sampleImprint())
	fl := &fakeLoader{entry: &cache.Entry{Envelope: cache.Envelope{Status: cache.StatusOK, FetchedAt: time.Unix(1700000000, 0).UTC(), Data: data}, Cached: true}}
	rec := do(t, newTestServer(t, fl, fakeLookups{}), http.MethodGet, "/v1/imprint?url=acme.de", nil)
	if rec.Header().Get("X-Cache") != "HIT" || decodeBody(t, rec)["cached"] != true {
		t.Fatalf("headers=%v body=%s", rec.Header(), rec.Body)
	}
}

func TestNegativeCachedImprintIs404(t *testing.T) {
	fl := &fakeLoader{entry: &cache.Entry{Envelope: cache.Envelope{Status: cache.StatusNotFound}, Cached: true}}
	rec := do(t, newTestServer(t, fl, fakeLookups{}), http.MethodGet, "/v1/imprint?url=acme.de", nil)
	if rec.Code != 404 || errCode(t, rec) != "imprint_not_found" || rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestAuth(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	for name, hdr := range map[string]map[string]string{
		"missing":      {"Authorization": ""},
		"wrong":        {"Authorization": "Bearer nope"},
		"wrong scheme": {"Authorization": "Basic " + token},
		"prefix only":  {"Authorization": "Bearer " + token[:10]},
		"empty bearer": {"Authorization": "Bearer "},
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", hdr)
			if rec.Code != 401 || errCode(t, rec) != "unauthorized" || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Fatal("token leaked")
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		if rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil); rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("probes need no auth", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz"} {
			if rec := do(t, s, http.MethodGet, p, map[string]string{"Authorization": ""}); rec.Code != 200 {
				t.Errorf("%s = %d", p, rec.Code)
			}
		}
	})
	t.Run("multiple tokens", func(t *testing.T) {
		l := fakeLookups{}
		l.imprintFn = func(context.Context, string) (*imprint.Imprint, error) { return sampleImprint(), nil }
		l.brandFn = func(context.Context, string) (*brand.Brand, error) { return &brand.Brand{}, nil }
		s2, err := New(&fakeLoader{}, l.Imprint(), l.Brand(), pingFunc(okPing), []string{"first-token-0123456789", "second-token-0123456789"})
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range []string{"first-token-0123456789", "second-token-0123456789"} {
			if rec := do(t, s2, http.MethodGet, "/v1/imprint?url=acme.de", map[string]string{"Authorization": "Bearer " + tk}); rec.Code != 200 {
				t.Errorf("token %s rejected", tk)
			}
		}
	})
	t.Run("disabled explicitly", func(t *testing.T) {
		s3 := newTestServer(t, &fakeLoader{}, fakeLookups{}, WithoutAuth())
		if rec := do(t, s3, http.MethodGet, "/v1/imprint?url=acme.de", map[string]string{"Authorization": ""}); rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("no tokens without opt-out is an error", func(t *testing.T) {
		l := fakeLookups{}
		if _, err := New(&fakeLoader{}, l.Imprint(), l.Brand(), pingFunc(okPing), nil); !errors.Is(err, ErrNoTokens) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestInvalidURLIs422(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	for _, target := range []string{
		"/v1/imprint", "/v1/imprint?url=", "/v1/imprint?url=http://localhost", "/v1/brand?url=ftp://x.de",
		"/v1/imprint?url=https://user:pw@acme.de", "/v1/brand?url=http://192.168.1.1/",
	} {
		rec := do(t, s, http.MethodGet, target, nil)
		if rec.Code != 422 || errCode(t, rec) != "invalid_url" {
			t.Errorf("%s: status=%d body=%s", target, rec.Code, rec.Body)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		retryAfter bool
	}{
		{"upstream", fmt.Errorf("x: %w", crawler.ErrUpstream), 502, "upstream_error", false},
		{"upstream timeout", fmt.Errorf("x: %w", crawler.ErrUpstreamTimeout), 504, "timeout", false},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), 504, "timeout", false},
		{"rate limited", fmt.Errorf("x: %w", crawler.ErrRateLimited), 503, "upstream_rate_limited", true},
		{"firecrawl account problem", fmt.Errorf("x: %w", crawler.ErrUpstreamUnavailable), 503, "upstream_unavailable", true},
		{"lock timeout", fmt.Errorf("x: %w", cache.ErrLockTimeout), 503, "unavailable", true},
		{"redis down", fmt.Errorf("x: %w", cache.ErrStoreUnavailable), 503, "unavailable", true},
		{"load shed", fmt.Errorf("x: %w", cache.ErrOverloaded), 503, "overloaded", true},
		{"shutting down", cache.ErrClosed, 503, "unavailable", true},
		{"cached upstream failure", &cache.CachedError{Code: crawler.CodeUpstream}, 502, "upstream_error", false},
		{"cached timeout", &cache.CachedError{Code: crawler.CodeTimeout}, 504, "timeout", false},
		{"domain does not resolve", fmt.Errorf("x: %w: %w", crawler.ErrHostNotFound, crawler.ErrUpstream), 422, "host_not_found", false},
		{"cached host not found", &cache.CachedError{Code: crawler.CodeHostNotFound}, 422, "host_not_found", false},
		{"client gone", context.Canceled, 499, "client_closed_request", false},
		{"unknown", errors.New("secret internal detail"), 500, "internal_error", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, &fakeLoader{err: tt.err}, fakeLookups{}, WithRetryAfter(7*time.Second))
			rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
			if rec.Code != tt.wantStatus || errCode(t, rec) != tt.wantCode {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); (got == "7") != tt.retryAfter || (got != "" && !tt.retryAfter) {
				t.Errorf("Retry-After = %q, want set=%v", got, tt.retryAfter)
			}
			if strings.Contains(rec.Body.String(), "secret internal detail") {
				t.Error("internal error detail leaked to the client")
			}
		})
	}
}

func TestMapErrorStatusCoverage(t *testing.T) {
	if got := mapError(urlnorm.ErrInvalidURL); got.status != 422 {
		t.Fatalf("%+v", got)
	}
}

func TestHandlerDeadlineIs504(t *testing.T) {
	s := newTestServer(t, &fakeLoader{block: true}, fakeLookups{}, WithHandlerTimeout(30*time.Millisecond))
	start := time.Now()
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	if rec.Code != 504 || errCode(t, rec) != "timeout" || time.Since(start) > 2*time.Second {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRefreshParameter(t *testing.T) {
	t.Run("ignored when disabled", func(t *testing.T) {
		fl := &fakeLoader{}
		do(t, newTestServer(t, fl, fakeLookups{}), http.MethodGet, "/v1/imprint?url=acme.de&refresh=true", nil)
		if fl.opts[0].Refresh {
			t.Fatal("refresh must be ignored while disabled")
		}
	})
	t.Run("honored when enabled", func(t *testing.T) {
		fl := &fakeLoader{}
		do(t, newTestServer(t, fl, fakeLookups{}, WithRefresh(true)), http.MethodGet, "/v1/imprint?url=acme.de&refresh=true", nil)
		if !fl.opts[0].Refresh {
			t.Fatal("refresh not passed through")
		}
	})
	t.Run("invalid value", func(t *testing.T) {
		rec := do(t, newTestServer(t, &fakeLoader{}, fakeLookups{}, WithRefresh(true)), http.MethodGet, "/v1/imprint?url=acme.de&refresh=maybe", nil)
		if rec.Code != 422 || errCode(t, rec) != "invalid_parameter" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("throttled header", func(t *testing.T) {
		data, _ := json.Marshal(sampleImprint())
		fl := &fakeLoader{entry: &cache.Entry{Envelope: cache.Envelope{Status: cache.StatusOK, Data: data}, Cached: true, RefreshThrottled: true}}
		rec := do(t, newTestServer(t, fl, fakeLookups{}, WithRefresh(true)), http.MethodGet, "/v1/imprint?url=acme.de&refresh=true", nil)
		if rec.Code != 200 || rec.Header().Get("X-Cache-Refresh") != "cooldown" || rec.Header().Get("X-Cache") != "HIT" {
			t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
		}
	})
}

func TestProbes(t *testing.T) {
	var pingErr error
	l := fakeLookups{}
	s, err := New(&fakeLoader{}, l.Imprint(), l.Brand(), pingFunc(func(context.Context) error { return pingErr }), []string{token})
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s, http.MethodGet, "/readyz", nil); rec.Code != 200 {
		t.Fatalf("ready = %d", rec.Code)
	}
	pingErr = errors.New("redis down")
	if rec := do(t, s, http.MethodGet, "/readyz", nil); rec.Code != 503 {
		t.Fatalf("ready with redis down = %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/healthz", nil); rec.Code != 200 {
		t.Fatalf("liveness must not depend on redis, got %d", rec.Code)
	}
	pingErr = nil
	s.MarkNotReady()
	if rec := do(t, s, http.MethodGet, "/readyz", nil); rec.Code != 503 || decodeBody(t, rec)["status"] != "shutting_down" {
		t.Fatalf("ready while shutting down = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodGet, "/healthz", nil); rec.Code != 200 {
		t.Fatalf("liveness during shutdown = %d", rec.Code)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	l := fakeLookups{imprintFn: func(context.Context, string) (*imprint.Imprint, error) { panic("kaboom") }}
	s := newTestServer(t, &fakeLoader{}, l)
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	if rec.Code != 500 || errCode(t, rec) != "internal_error" || strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodGet, "/healthz", nil); rec.Code != 200 {
		t.Fatal("server must keep serving after a panic")
	}
}

func TestRequestID(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	rec := do(t, s, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": "abc-123_x.y"})
	if rec.Header().Get("X-Request-Id") != "abc-123_x.y" {
		t.Errorf("valid id must be echoed, got %q", rec.Header().Get("X-Request-Id"))
	}
	rec = do(t, s, http.MethodGet, "/healthz", map[string]string{"X-Request-Id": "bad id\twith spaces"})
	if got := rec.Header().Get("X-Request-Id"); got == "" || strings.Contains(got, " ") || len(got) != 16 {
		t.Errorf("invalid id must be replaced, got %q", got)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	s := newTestServer(t, &fakeLoader{}, fakeLookups{})
	if rec := do(t, s, http.MethodGet, "/nope", nil); rec.Code != 404 || errCode(t, rec) != "not_found" {
		t.Fatalf("404: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodPost, "/v1/imprint?url=acme.de", nil); rec.Code != 405 || errCode(t, rec) != "method_not_allowed" {
		t.Fatalf("405: %d %s", rec.Code, rec.Body)
	}
}

type recordedRequest struct {
	route, method string
	status        int
}

type recObserver struct {
	mu       sync.Mutex
	reqs     []recordedRequest
	rejected []string
}

func (r *recObserver) Rejected(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejected = append(r.rejected, reason)
}

func (r *recObserver) ObserveRequest(route, method string, status int, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{route, method, status})
}

func TestMetricsUseRouteTemplates(t *testing.T) {
	obs := &recObserver{}
	s := newTestServer(t, &fakeLoader{}, fakeLookups{}, WithObserver(obs))
	do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	do(t, s, http.MethodGet, "/v1/imprint?url=localhost", nil)
	do(t, s, http.MethodGet, "/does/not/exist/123", nil)
	got := obs.reqs
	if len(got) != 3 || got[0] != (recordedRequest{"/v1/imprint", "GET", 200}) || got[1].status != 422 || got[2].route == "/does/not/exist/123" {
		t.Fatalf("observed = %+v", got)
	}
}

// End to end with the real cache over miniredis (single node; miniredis has no cluster mode).
func TestEndToEndWithRealCache(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	store := cache.NewStore(rdb, cache.WithLoadTimeout(2*time.Second), cache.WithLockMargin(time.Second),
		cache.WithClassifier(func(err error) cache.Verdict {
			switch {
			case errors.Is(err, imprint.ErrNotFound):
				return cache.Verdict{Status: cache.StatusNotFound, Code: "not_found"}
			case crawler.ErrorCode(err) == crawler.CodeUpstream:
				return cache.Verdict{Status: cache.StatusError, Code: crawler.CodeUpstream}
			}
			return cache.Verdict{}
		}))

	var calls int
	var mu sync.Mutex
	l := fakeLookups{imprintFn: func(_ context.Context, host string) (*imprint.Imprint, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch host {
		case "gone.de":
			return nil, imprint.ErrNotFound
		case "broken.de":
			return nil, fmt.Errorf("scrape: %w", crawler.ErrUpstream)
		}
		return sampleImprint(), nil
	}}
	s, err := New(store, l.Imprint(), l.Brand(), store, []string{token})
	if err != nil {
		t.Fatal(err)
	}

	first := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	second := do(t, s, http.MethodGet, "/v1/imprint?url=https://www.acme.de/other", nil)
	if first.Header().Get("X-Cache") != "MISS" || second.Header().Get("X-Cache") != "HIT" || calls != 1 {
		t.Fatalf("first=%s second=%s calls=%d", first.Header().Get("X-Cache"), second.Header().Get("X-Cache"), calls)
	}
	if decodeBody(t, first)["cached"] != false || decodeBody(t, second)["cached"] != true {
		t.Fatal("cached flag wrong")
	}
	if decodeBody(t, second)["fetched_at"] != decodeBody(t, first)["fetched_at"] {
		t.Fatal("fetched_at must be the load time, not the request time")
	}

	for i := range 2 { // negative results are cached: upstream is called once
		rec := do(t, s, http.MethodGet, "/v1/imprint?url=gone.de", nil)
		if rec.Code != 404 {
			t.Fatalf("gone.de #%d = %d %s", i, rec.Code, rec.Body)
		}
		rec = do(t, s, http.MethodGet, "/v1/imprint?url=broken.de", nil)
		if rec.Code != 502 || errCode(t, rec) != "upstream_error" {
			t.Fatalf("broken.de #%d = %d %s", i, rec.Code, rec.Body)
		}
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3 (acme, gone, broken once each)", calls)
	}

	if rec := do(t, s, http.MethodGet, "/readyz", nil); rec.Code != 200 {
		t.Fatalf("readyz = %d", rec.Code)
	}
	mr.Close()
	if rec := do(t, s, http.MethodGet, "/readyz", nil); rec.Code != 503 {
		t.Fatalf("readyz with redis down = %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/healthz", nil); rec.Code != 200 {
		t.Fatalf("healthz with redis down = %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil); rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("lookup with redis down = %d %v", rec.Code, rec.Header())
	}
}
