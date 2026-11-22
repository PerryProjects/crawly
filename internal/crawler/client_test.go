package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firecrawl/firecrawl/apps/go-sdk"
)

type fakeScraper struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, call int) (*firecrawl.Document, error)
}

func (f *fakeScraper) Scrape(ctx context.Context, _ string, _ *firecrawl.ScrapeOptions) (*firecrawl.Document, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(ctx, n)
}

type obsRecorder struct {
	mu  sync.Mutex
	got []string
}

func (o *obsRecorder) UpstreamCall(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, outcome)
}

func rateLimit() error {
	return &firecrawl.RateLimitError{FirecrawlError: firecrawl.FirecrawlError{StatusCode: 429, Message: "slow down"}}
}

func httpErr(code int) error {
	return &firecrawl.FirecrawlError{StatusCode: code, Message: "boom"}
}

type sleeps struct {
	mu sync.Mutex
	ds []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ds = append(s.ds, d)
	return nil
}

func TestRetryAndErrorMapping(t *testing.T) {
	okDoc := &firecrawl.Document{Markdown: "hello"}
	tests := []struct {
		name      string
		errs      []error // per call; nil entry = success
		wantCalls int
		wantErr   error
		outcomes  []string
	}{
		{"success", []error{nil}, 1, nil, []string{"ok"}},
		{"429 then ok", []error{rateLimit(), nil}, 2, nil, []string{"rate_limited", "ok"}},
		{"500 then ok", []error{httpErr(500), nil}, 2, nil, []string{"outage", "ok"}},
		{"429 forever gives up after max retries", []error{rateLimit(), rateLimit(), rateLimit(), nil}, 3, ErrRateLimited, []string{"rate_limited", "rate_limited", "rate_limited"}},
		{"503 forever is an outage", []error{httpErr(503), httpErr(503), httpErr(503)}, 3, ErrUpstreamUnavailable, []string{"outage", "outage", "outage"}},
		{"firecrawl 408 is a timeout, not retried", []error{httpErr(http.StatusRequestTimeout)}, 1, ErrUpstreamTimeout, []string{"timeout"}},
		{"400 is not retried", []error{httpErr(400)}, 1, ErrUpstream, []string{"error"}},
		{"200 success:false is not retried", []error{&firecrawl.FirecrawlError{StatusCode: 200, ErrorCode: "SCRAPE_ALL_ENGINES_FAILED"}}, 1, ErrUpstream, []string{"error"}},
		{"DNS failure is host not found", []error{&firecrawl.FirecrawlError{StatusCode: 200, ErrorCode: "SCRAPE_DNS_RESOLUTION_ERROR"}}, 1, ErrHostNotFound, []string{"error"}},
		{"timeout is not retried", []error{context.DeadlineExceeded}, 1, ErrUpstreamTimeout, []string{"timeout"}},
		{"401 is an account error", []error{&firecrawl.AuthenticationError{FirecrawlError: firecrawl.FirecrawlError{StatusCode: 401}}}, 1, ErrUpstreamUnavailable, []string{"account_error"}},
		{"402 no credits is an account error", []error{httpErr(http.StatusPaymentRequired)}, 1, ErrUpstreamUnavailable, []string{"account_error"}},
		{"pending terms is an account error", []error{&firecrawl.FirecrawlError{StatusCode: 403, RequiresAction: &firecrawl.RequiresAction{Type: "accept_terms"}}}, 1, ErrUpstreamUnavailable, []string{"account_error"}},
		{"500 with a site error code is a site failure, not retried", []error{&firecrawl.FirecrawlError{StatusCode: 500, ErrorCode: "SCRAPE_SSL_ERROR"}}, 1, ErrUpstream, []string{"error"}},
		{"403 without action is site specific", []error{httpErr(http.StatusForbidden)}, 1, ErrUpstream, []string{"error"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fakeScraper{fn: func(_ context.Context, call int) (*firecrawl.Document, error) {
				if err := tt.errs[call-1]; err != nil {
					return nil, err
				}
				return okDoc, nil
			}}
			obs, sl := &obsRecorder{}, &sleeps{}
			c := New(fs, WithObserver(obs), WithSleep(sl.sleep), WithBackoff(100*time.Millisecond))
			page, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true})
			if tt.wantErr == nil {
				if err != nil || page.Markdown != "hello" {
					t.Fatalf("page=%+v err=%v", page, err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if fs.calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", fs.calls, tt.wantCalls)
			}
			if len(obs.got) != len(tt.outcomes) {
				t.Fatalf("outcomes = %v, want %v", obs.got, tt.outcomes)
			}
			for i := range obs.got {
				if obs.got[i] != tt.outcomes[i] {
					t.Errorf("outcomes = %v, want %v", obs.got, tt.outcomes)
				}
			}
			if want := tt.wantCalls - 1; len(sl.ds) != want {
				t.Errorf("sleeps = %v, want %d", sl.ds, want)
			}
			for i, d := range sl.ds { // exponential, equal jitter: [base*2^i/2, base*2^i]
				hi := 100 * time.Millisecond << i
				if d < hi/2 || d > hi {
					t.Errorf("backoff %d = %s, want within [%s, %s]", i, d, hi/2, hi)
				}
			}
		})
	}
}

func TestPerCallTimeoutIsUpstreamTimeout(t *testing.T) {
	fs := &fakeScraper{fn: func(ctx context.Context, _ int) (*firecrawl.Document, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := New(fs, WithTimeout(20*time.Millisecond))
	_, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{})
	if !errors.Is(err, ErrUpstreamTimeout) || ErrorCode(err) != CodeTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestCallerCancellationIsNotAnUpstreamError(t *testing.T) {
	fs := &fakeScraper{fn: func(ctx context.Context, _ int) (*firecrawl.Document, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	obs := &obsRecorder{}
	c := New(fs, WithObserver(obs))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := c.Scrape(ctx, "https://example.de/", ScrapeRequest{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUpstream) || ErrorCode(err) != "" {
		t.Fatalf("err = %v", err)
	}
	if len(obs.got) != 1 || obs.got[0] != "canceled" {
		t.Fatalf("outcomes = %v", obs.got)
	}
}

func TestNoRetryWhenBudgetIsTooSmall(t *testing.T) {
	fs := &fakeScraper{fn: func(context.Context, int) (*firecrawl.Document, error) { return nil, rateLimit() }}
	sl := &sleeps{}
	c := New(fs, WithSleep(sl.sleep), WithBackoff(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Scrape(ctx, "https://example.de/", ScrapeRequest{})
	if !errors.Is(err, ErrRateLimited) || fs.calls != 1 || len(sl.ds) != 0 {
		t.Fatalf("err=%v calls=%d sleeps=%v", err, fs.calls, sl.ds)
	}
}

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	var cur, peak atomic.Int32
	fs := &fakeScraper{fn: func(ctx context.Context, _ int) (*firecrawl.Document, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		cur.Add(-1)
		return &firecrawl.Document{}, nil
	}}
	c := New(fs, WithMaxConcurrency(2))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() > 2 || peak.Load() == 0 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak.Load())
	}
}

func TestSemaphoreWaitHonorsContext(t *testing.T) {
	release := make(chan struct{})
	fs := &fakeScraper{fn: func(ctx context.Context, _ int) (*firecrawl.Document, error) {
		<-release
		return &firecrawl.Document{}, nil
	}}
	c := New(fs, WithMaxConcurrency(1))
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = c.Scrape(context.Background(), "https://a.de/", ScrapeRequest{})
	}()
	<-started
	for { // wait until the first call holds the slot
		fs.mu.Lock()
		n := fs.calls
		fs.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Scrape(ctx, "https://b.de/", ScrapeRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(release)
}

func TestToPage(t *testing.T) {
	doc := &firecrawl.Document{
		Markdown: "# x",
		Links:    []string{"https://example.de/impressum"},
		JSON:     map[string]any{"company_name": "ACME"},
		Metadata: map[string]any{"statusCode": float64(404), "url": "https://example.de/final"},
	}
	p := toPage("https://example.de/", doc)
	if p.URL != "https://example.de/final" || p.StatusCode != http.StatusNotFound || p.JSON["company_name"] != "ACME" || len(p.Links) != 1 {
		t.Fatalf("%+v", p)
	}
	if p := toPage("https://x.de/", &firecrawl.Document{JSON: "not an object"}); p.JSON != nil || p.URL != "https://x.de/" {
		t.Fatalf("%+v", p)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"garbage":                       0,
		"Thu, 01 Jan 2026 12:00:30 GMT": 30 * time.Second,
		"Thu, 01 Jan 2026 11:00:00 GMT": 0,
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

// The tests below run the real SDK against an httptest server. They verify the
// request body we send and how real HTTP errors surface; the server is a fake,
// so they do NOT prove compatibility with the live Firecrawl API.

func newSDKClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	sdk, err := NewSDK("fc-test-key", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(sdk, opts...)
}

func TestSDKRequestShape(t *testing.T) {
	var body map[string]any
	var auth string
	c := newSDKClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v2/scrape" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"# Impressum","links":["https://example.de/impressum"],`+
			`"json":{"company_name":"ACME GmbH"},"metadata":{"statusCode":200}}}`)
	}, WithLocation("DE", []string{"de"}))
	page, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{
		Markdown: true, Links: true, Branding: true, FullPage: true,
		JSON: &JSONExtract{Prompt: "extract", Schema: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Markdown != "# Impressum" || page.StatusCode != http.StatusOK || page.JSON["company_name"] != "ACME GmbH" {
		t.Fatalf("page = %+v", page)
	}
	if auth != "Bearer fc-test-key" {
		t.Errorf("auth header = %q", auth)
	}
	if body["url"] != "https://example.de/" || body["onlyMainContent"] != false {
		t.Errorf("body = %v", body)
	}
	formats, _ := body["formats"].([]any)
	if len(formats) != 4 || formats[0] != "markdown" || formats[1] != "links" || formats[2] != "branding" {
		t.Fatalf("formats = %v", formats)
	}
	jf, _ := formats[3].(map[string]any)
	if jf["type"] != "json" || jf["prompt"] != "extract" || jf["schema"] == nil {
		t.Errorf("json format = %v", formats[3])
	}
	if body["timeout"] == nil {
		t.Error("firecrawl timeout missing")
	}
	loc, _ := body["location"].(map[string]any)
	if langs, _ := loc["languages"].([]any); loc["country"] != "DE" || len(langs) != 1 || langs[0] != "de" {
		t.Errorf("location = %v", body["location"])
	}
}

func TestSDKRetryAfterIsHonored(t *testing.T) {
	var n atomic.Int32
	sl := &sleeps{}
	c := newSDKClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"success":false,"error":"rate limit"}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"ok"}}`)
	}, WithSleep(sl.sleep), WithBackoff(10*time.Millisecond))
	if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true}); err != nil {
		t.Fatal(err)
	}
	if len(sl.ds) != 1 || sl.ds[0] != 3*time.Second {
		t.Fatalf("sleeps = %v, want [3s] from Retry-After", sl.ds)
	}
}

func TestSDKTransportErrorIsRetried(t *testing.T) {
	var n atomic.Int32
	c := newSDKClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close() // abrupt close -> transport error
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"ok"}}`)
	}, WithSleep((&sleeps{}).sleep))
	page, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true})
	if err != nil || page.Markdown != "ok" || n.Load() != 2 {
		t.Fatalf("page=%+v err=%v calls=%d", page, err, n.Load())
	}
}

func TestSDK5xxIsAnOutageNotASiteFailure(t *testing.T) {
	c := newSDKClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"bad gateway"}`)
	}, WithSleep((&sleeps{}).sleep))
	_, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{})
	var fe *firecrawl.FirecrawlError
	if !errors.Is(err, ErrUpstreamUnavailable) || errors.Is(err, ErrUpstream) || !errors.As(err, &fe) || fe.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorCode(t *testing.T) {
	if ErrorCode(fmt.Errorf("%w: %w", ErrHostNotFound, ErrUpstream)) != CodeHostNotFound {
		t.Fatal("host not found code")
	}
	if ErrorCode(ErrUpstreamUnavailable) != CodeUnavailable || errors.Is(ErrUpstreamUnavailable, ErrUpstream) {
		t.Fatal("account errors must not count as site failures")
	}
	if ErrorCode(ErrRateLimited) != CodeRateLimited || ErrorCode(ErrUpstream) != CodeUpstream || ErrorCode(errors.New("x")) != "" {
		t.Fatal("codes")
	}
}

func TestNoLocationByDefault(t *testing.T) {
	var body map[string]any
	c := newSDKClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"x"}}`)
	})
	if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["location"]; ok {
		t.Errorf("location must be absent unless configured: %v", body["location"])
	}
}

func TestFreshContextBypassesFirecrawlCache(t *testing.T) {
	var bodies []map[string]any
	c := newSDKClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"x"}}`)
	})
	if _, err := c.Scrape(WithFresh(context.Background()), "https://example.de/", ScrapeRequest{Markdown: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true}); err != nil {
		t.Fatal(err)
	}
	if v, ok := bodies[0]["maxAge"]; !ok || v != float64(0) {
		t.Errorf("fresh call: maxAge = %v (present %v), want 0", v, ok)
	}
	if _, ok := bodies[1]["maxAge"]; ok {
		t.Errorf("normal calls leave Firecrawl's default max age: %v", bodies[1]["maxAge"])
	}
}

func TestNoRetryThatTheDeadlineWouldCut(t *testing.T) {
	fs := &fakeScraper{fn: func(context.Context, int) (*firecrawl.Document, error) { return nil, httpErr(500) }}
	c := New(fs, WithTimeout(time.Second), WithSleep((&sleeps{}).sleep), WithBackoff(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond) // less than one more call
	defer cancel()
	_, err := c.Scrape(ctx, "https://example.de/", ScrapeRequest{})
	if fs.calls != 1 || !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("calls=%d err=%v; want one call and the classified error", fs.calls, err)
	}
}

func TestPerRequestTimeout(t *testing.T) {
	var body map[string]any
	c := newSDKClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"x"}}`)
	}, WithTimeout(30*time.Second))
	if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Branding: true, Timeout: 50 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if body["timeout"] != float64(45000) { // 90 % of the request's own timeout
		t.Fatalf("firecrawl timeout = %v", body["timeout"])
	}
}

func TestStoreInCacheOption(t *testing.T) {
	for _, tt := range []struct {
		name  string
		opts  []Option
		want  any
		isSet bool
	}{
		{"default leaves Firecrawl's default", nil, nil, false},
		{"disabled sends false", []Option{WithStoreInCache(false)}, false, true},
		{"enabled leaves Firecrawl's default", []Option{WithStoreInCache(true)}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]any
			c := newSDKClient(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"x"}}`)
			}, tt.opts...)
			if _, err := c.Scrape(context.Background(), "https://example.de/", ScrapeRequest{Markdown: true}); err != nil {
				t.Fatal(err)
			}
			v, ok := body["storeInCache"]
			if ok != tt.isSet || v != tt.want {
				t.Fatalf("storeInCache = %v (set %v), want %v (set %v)", v, ok, tt.want, tt.isSet)
			}
		})
	}
}
