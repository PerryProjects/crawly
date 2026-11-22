package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/perryprojects/siteinfo/internal/brand"
	"github.com/perryprojects/siteinfo/internal/cache"
	"github.com/perryprojects/siteinfo/internal/crawler"
	"github.com/perryprojects/siteinfo/internal/imprint"
	"github.com/perryprojects/siteinfo/internal/observability"
)

// debugServer wires the real cache over miniredis and lookups that write to the request trace.
func debugServer(t *testing.T, opts ...Option) (srv *Server, lookupCalls *int) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	store := cache.NewStore(rdb, cache.WithLoadTimeout(2*time.Second), cache.WithLockTTL(time.Second),
		cache.WithClassifier(func(err error) cache.Verdict {
			if errors.Is(err, crawler.ErrUpstream) {
				return cache.Verdict{Status: cache.StatusError, Code: crawler.CodeUpstream}
			}
			return cache.Verdict{}
		}))
	calls := new(int)
	l := mockLookups{
		imprintFn: func(ctx context.Context, host string) (*imprint.Imprint, error) {
			*calls++
			observability.TraceFrom(ctx).Add("crawler", "firecrawl call", "url", "https://"+host+"/", "outcome", "ok")
			if host == "broken.de" {
				return nil, fmt.Errorf("scrape: %w", crawler.ErrUpstream)
			}
			return sampleImprint(), nil
		},
		brandFn: func(context.Context, string) (*brand.Brand, error) { return &brand.Brand{}, nil },
	}
	cfg := map[string]any{"auth": map[string]any{"tokens": "***"}, "server": map[string]any{"handler_timeout": "45s"}}
	s, err := New(store, l.Imprint(), l.Brand(), store, []string{token}, append([]Option{WithDebug(cfg)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s, calls
}

func debugOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	d, ok := decodeBody(t, rec)["_debug"].(map[string]any)
	if !ok {
		t.Fatalf("no _debug in %s", rec.Body)
	}
	return d
}

func stages(d map[string]any) []string {
	var out []string
	for _, e := range d["trace"].([]any) {
		m := e.(map[string]any)
		out = append(out, m["stage"].(string)+": "+m["message"].(string))
	}
	return out
}

func hasStage(d map[string]any, sub string) bool {
	for _, s := range stages(d) {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func testDebugDisabledByDefault(t *testing.T) {
	s := newTestServer(t, &mockLoader{}, mockLookups{})
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de&debug=1", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "_debug") {
		t.Fatalf("debug=1 must be ignored while debug mode is off: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []struct{ method, path string }{{http.MethodGet, "/v1/debug/config"}} {
		if rec := do(t, s, p.method, p.path, nil); rec.Code != 404 && rec.Code != 405 {
			t.Errorf("%s %s = %d, want it absent", p.method, p.path, rec.Code)
		}
	}
}

func testDebugBlockOnlyWhenRequested(t *testing.T) {
	s, _ := debugServer(t)
	if rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil); strings.Contains(rec.Body.String(), "_debug") {
		t.Fatal("no _debug without debug=1")
	}
}

func testDebugTraceMissThenHit(t *testing.T) {
	s, calls := debugServer(t)
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=https://www.ACME.de/x&debug=1", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	d := debugOf(t, rec)
	for _, want := range []string{"urlnorm: input normalized", "cache: miss", "cache: lock acquired", "crawler: firecrawl call", "cache: upstream finished", "cache: storing result"} {
		if !hasStage(d, want) {
			t.Errorf("trace lacks %q: %v", want, stages(d))
		}
	}
	if d["host"] != "acme.de" || d["homepage"] != "https://acme.de/" || d["kind"] != "imprint" || d["duration_ms"].(float64) <= 0 {
		t.Errorf("block = %v", d)
	}
	if d["note"] != nil {
		t.Errorf("a loading request needs no note: %v", d["note"])
	}
	ci := d["cache"].(map[string]any)
	if ci["exists"] != true || ci["status"] != "ok" || ci["cache_key"] != "imprint:v2:{acme.de}" || ci["ttl_seconds"].(float64) < 29*24*3600 {
		t.Errorf("cache info = %v", ci)
	}
	// Trace events are ordered in time.
	prev := -1.0
	for _, e := range d["trace"].([]any) {
		at := e.(map[string]any)["at_ms"].(float64)
		if at < prev {
			t.Errorf("trace not ordered: %v", d["trace"])
		}
		prev = at
	}

	rec = do(t, s, http.MethodGet, "/v1/imprint?url=acme.de&debug=1", nil)
	d = debugOf(t, rec)
	if *calls != 1 || !hasStage(d, "cache: hit") || hasStage(d, "crawler:") {
		t.Fatalf("second request must be a traced hit: calls=%d %v", *calls, stages(d))
	}
	if note, _ := d["note"].(string); !strings.Contains(note, "No upstream call") {
		t.Errorf("a hit explains why there is no load trace, note = %q", note)
	}
}

func testDebugBlockOnErrors(t *testing.T) {
	s, _ := debugServer(t)
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=broken.de&debug=1", nil)
	if rec.Code != 502 || errCode(t, rec) != "upstream_error" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := debugOf(t, rec)
	if !hasStage(d, "crawler: firecrawl call") || !hasStage(d, "server: request failed") {
		t.Errorf("trace of a failed load: %v", stages(d))
	}
	if ci := d["cache"].(map[string]any); ci["status"] != "error" || ci["error_code"] != "upstream_error" {
		t.Errorf("the negative cache entry must be visible: %v", ci)
	}

	rec = do(t, s, http.MethodGet, "/v1/imprint?url=http://localhost&debug=1", nil)
	if rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	d = debugOf(t, rec)
	if d["input"] != "http://localhost" || !hasStage(d, "urlnorm: input rejected") {
		t.Errorf("rejected input must be traced: %v", d)
	}
	if rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil); strings.Contains(rec.Body.String(), "_debug") {
		t.Fatal("errors without debug=1 stay clean")
	}
}

func testDebugFlushCache(t *testing.T) {
	s, calls := debugServer(t)
	do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil)
	if *calls != 1 {
		t.Fatalf("calls = %d", *calls)
	}

	rec := do(t, s, http.MethodDelete, "/v1/cache?url=https://www.acme.de/", nil)
	body := decodeBody(t, rec)
	if rec.Code != 200 || body["host"] != "acme.de" || body["deleted_keys"] != float64(1) {
		t.Fatalf("flush: %d %v", rec.Code, body)
	}
	if rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de", nil); rec.Header().Get("X-Cache") != "MISS" || *calls != 2 {
		t.Fatalf("after flush: X-Cache=%s calls=%d", rec.Header().Get("X-Cache"), *calls)
	}
	if rec := do(t, s, http.MethodDelete, "/v1/cache?url=acme.de&kind=nonsense", nil); rec.Code != 422 || errCode(t, rec) != "invalid_parameter" {
		t.Fatalf("bad kind: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodDelete, "/v1/cache?url=localhost", nil); rec.Code != 422 {
		t.Fatalf("bad url: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodDelete, "/v1/cache?url=acme.de", map[string]string{"Authorization": ""}); rec.Code != 401 {
		t.Fatalf("flush must require auth: %d", rec.Code)
	}
}

func testDebugConfigEndpoint(t *testing.T) {
	s, _ := debugServer(t)
	rec := do(t, s, http.MethodGet, "/v1/debug/config", nil)
	body := decodeBody(t, rec)
	if rec.Code != 200 || body["config"].(map[string]any)["server"].(map[string]any)["handler_timeout"] != "45s" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rt := body["runtime"].(map[string]any)
	if rt["go_version"] == nil || rt["goroutines"].(float64) < 1 || rt["uptime_seconds"] == nil {
		t.Errorf("runtime = %v", rt)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatal("the bearer token must never be echoed")
	}
	if rec := do(t, s, http.MethodGet, "/v1/debug/config", map[string]string{"Authorization": "Bearer nope"}); rec.Code != 401 {
		t.Fatalf("config must require auth: %d", rec.Code)
	}
}

func testDebugWithLoaderWithoutInspection(t *testing.T) {
	s := newTestServer(t, &mockLoader{}, mockLookups{}, WithDebug(map[string]any{}))
	rec := do(t, s, http.MethodGet, "/v1/imprint?url=acme.de&debug=1", nil)
	d := debugOf(t, rec)
	if rec.Code != 200 || d["cache"] != nil {
		t.Fatalf("%d %v", rec.Code, d)
	}
	if rec := do(t, s, http.MethodDelete, "/v1/cache?url=acme.de", nil); rec.Code != 404 {
		t.Fatalf("flush on a loader without support = %d", rec.Code)
	}
}

func TestDebugMode(t *testing.T) {
	t.Run("debug disabled by default", testDebugDisabledByDefault)
	t.Run("debug block only when requested", testDebugBlockOnlyWhenRequested)
	t.Run("debug trace miss then hit", testDebugTraceMissThenHit)
	t.Run("debug block on errors", testDebugBlockOnErrors)
	t.Run("debug flush cache", testDebugFlushCache)
	t.Run("debug config endpoint", testDebugConfigEndpoint)
	t.Run("debug with loader without inspection", testDebugWithLoaderWithoutInspection)
}
