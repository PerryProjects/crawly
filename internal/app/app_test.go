package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/config"
	"github.com/OWNER/siteinfo/internal/crawler"
	"github.com/OWNER/siteinfo/internal/imprint"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want cache.Verdict
	}{
		{"not found", fmt.Errorf("x: %w", imprint.ErrNotFound), cache.Verdict{Status: cache.StatusNotFound, Code: "not_found"}},
		{"upstream", fmt.Errorf("x: %w", crawler.ErrUpstream), cache.Verdict{Status: cache.StatusError, Code: "upstream_error"}},
		{"upstream timeout", fmt.Errorf("x: %w", crawler.ErrUpstreamTimeout), cache.Verdict{Status: cache.StatusError, Code: "upstream_timeout"}},
		{"rate limited is never cached", fmt.Errorf("x: %w", crawler.ErrRateLimited), cache.Verdict{}},
		{"load deadline is never cached", context.DeadlineExceeded, cache.Verdict{}},
		{"cancellation is never cached", context.Canceled, cache.Verdict{}},
		{"unknown", errors.New("boom"), cache.Verdict{}},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("%s: got %+v want %+v", tt.name, got, tt.want)
		}
	}
}

// fakeFirecrawl emulates POST /v2/scrape for acme.de. It does NOT prove
// compatibility with the real service.
type fakeFirecrawl struct {
	calls atomic.Int32
	gate  chan struct{} // when non-nil, the imprint page waits for it
}

func (f *fakeFirecrawl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(b, &req)
	f.calls.Add(1)
	switch req.URL {
	case "https://acme.de/":
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"[Impressum](https://acme.de/impressum)","links":["https://acme.de/impressum","https://acme.de/datenschutz"],"metadata":{"statusCode":200,"url":"https://acme.de/"}}}`)
	case "https://acme.de/impressum":
		if f.gate != nil {
			<-f.gate
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"# Impressum\nACME GmbH\n10115 Berlin\nUSt-IdNr. DE123456789","json":{"company_name":"ACME GmbH","vat_id":"DE123456789","country":"DE","address":{"postal_code":"10115","city":"Berlin"}},"metadata":{"statusCode":200,"url":"https://acme.de/impressum"}}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success":false,"error":"not found"}`)
	}
}

func testConfig(t *testing.T, redisAddr, firecrawlURL string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Env = "dev"
	cfg.Server.Addr, cfg.Server.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0"
	cfg.Server.ShutdownDelay = 150 * time.Millisecond
	cfg.Auth.Tokens = []string{"app-test-token-0123456789"}
	cfg.Redis.Mode, cfg.Redis.Addrs = "single", []string{redisAddr}
	cfg.Firecrawl.APIKey, cfg.Firecrawl.APIURL = "fc-test", firecrawlURL
	cfg.Firecrawl.BackoffBase = time.Millisecond
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func get(t *testing.T, url, token string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, resp.Header, m
}

// Full stack: real wiring, real Firecrawl SDK against a fake, miniredis, real listeners.
func TestEndToEndAndGracefulShutdown(t *testing.T) {
	mr := miniredis.RunT(t)
	fc := &fakeFirecrawl{gate: make(chan struct{})}
	fcSrv := httptest.NewServer(fc)
	t.Cleanup(fcSrv.Close)

	a, err := New(testConfig(t, mr.Addr(), fcSrv.URL), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()

	base := "http://" + a.APIAddr()
	const tok = "app-test-token-0123456789"
	if code, _, _ := get(t, base+"/readyz", ""); code != 200 {
		t.Fatalf("readyz = %d", code)
	}
	if code, _, _ := get(t, base+"/v1/imprint?url=acme.de", ""); code != 401 {
		t.Fatalf("unauthenticated = %d", code)
	}

	// Start a slow load, then begin shutdown while it is in flight.
	type result struct {
		code int
		hdr  http.Header
		body map[string]any
	}
	inflight := make(chan result, 1)
	go func() {
		code, hdr, body := get(t, base+"/v1/imprint?url=https://www.acme.de/x", tok)
		inflight <- result{code, hdr, body}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for fc.calls.Load() < 2 { // homepage + imprint page (blocked on the gate)
		if time.Now().After(deadline) {
			t.Fatal("load never reached the imprint page")
		}
		time.Sleep(time.Millisecond)
	}

	stop() // SIGTERM equivalent
	time.Sleep(50 * time.Millisecond)
	if code, _, body := get(t, base+"/readyz", ""); code != 503 || body["status"] != "shutting_down" {
		t.Fatalf("readyz during shutdown delay = %d %v", code, body)
	}
	if code, _, _ := get(t, base+"/healthz", ""); code != 200 {
		t.Fatalf("healthz during shutdown = %d", code)
	}
	close(fc.gate) // let the in-flight request finish

	r := <-inflight
	if r.code != 200 || r.hdr.Get("X-Cache") != "MISS" || r.body["company_name"] != "ACME GmbH" || r.body["vat_id"] != "DE123456789" {
		t.Fatalf("in-flight request = %d %v %v", r.code, r.hdr, r.body)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if !mr.Exists("imprint:v1:{acme.de}") {
		t.Fatal("result was not cached")
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("API still accepting connections after shutdown")
	}
}

func TestServeAndCacheAcrossRequests(t *testing.T) {
	mr := miniredis.RunT(t)
	fc := &fakeFirecrawl{}
	fcSrv := httptest.NewServer(fc)
	t.Cleanup(fcSrv.Close)
	cfg := testConfig(t, mr.Addr(), fcSrv.URL)
	cfg.Server.ShutdownDelay = 0

	a, err := New(cfg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()
	t.Cleanup(func() { stop(); <-runDone })

	base, tok := "http://"+a.APIAddr(), "app-test-token-0123456789"
	code, hdr, body := get(t, base+"/v1/imprint?url=acme.de", tok)
	if code != 200 || hdr.Get("X-Cache") != "MISS" || body["cached"] != false {
		t.Fatalf("first = %d %v %v", code, hdr, body)
	}
	callsAfterFirst := fc.calls.Load()
	code, hdr, body = get(t, base+"/v1/imprint?url=acme.de", tok)
	if code != 200 || hdr.Get("X-Cache") != "HIT" || body["cached"] != true || fc.calls.Load() != callsAfterFirst {
		t.Fatalf("second = %d %v %v calls=%d", code, hdr, body, fc.calls.Load())
	}

	// Unknown site: Firecrawl fails for the homepage -> 502, then served from the negative cache.
	code, _, body = get(t, base+"/v1/imprint?url=unknown.de", tok)
	if code != 502 {
		t.Fatalf("unknown site = %d %v", code, body)
	}
	before := fc.calls.Load()
	if code, hdr, _ = get(t, base+"/v1/imprint?url=unknown.de", tok); code != 502 || hdr.Get("X-Cache") != "HIT" || fc.calls.Load() != before {
		t.Fatalf("negative cache: code=%d hdr=%v calls %d->%d", code, hdr, before, fc.calls.Load())
	}

	// Metrics are on the separate port and carry no secrets.
	resp, err := http.Get("http://" + a.MetricsAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"siteinfo_cache_lookups_total", "siteinfo_upstream_calls_total", "siteinfo_http_request_duration_seconds", "siteinfo_inflight_loads"} {
		if !strings.Contains(string(mb), want) {
			t.Errorf("metric %s missing", want)
		}
	}
	if strings.Contains(string(mb), tok) || strings.Contains(string(mb), "fc-test") {
		t.Error("secret in metrics output")
	}
	if code, _, _ := get(t, "http://"+a.APIAddr()+"/metrics", tok); code != 404 {
		t.Errorf("/metrics must not be served on the API port, got %d", code)
	}
}

func TestNewFailsFastOnBusyPort(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := testConfig(t, mr.Addr(), "http://127.0.0.1:1")
	a, err := New(cfg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer a.apiLn.Close()
	defer a.metricsLn.Close()
	cfg2 := *cfg
	cfg2.Server.Addr = a.APIAddr()
	if _, err := New(&cfg2, zap.NewNop()); err == nil {
		t.Fatal("expected a listen error")
	}
}
