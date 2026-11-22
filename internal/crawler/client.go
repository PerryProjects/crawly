package crawler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/firecrawl/firecrawl/apps/go-sdk"
	"github.com/firecrawl/firecrawl/apps/go-sdk/option"
	"go.uber.org/zap"

	"github.com/perryprojects/siteinfo/internal/observability"
)

// Scraper is the part of *firecrawl.Client this package needs.
type Scraper interface {
	Scrape(ctx context.Context, url string, opts *firecrawl.ScrapeOptions) (*firecrawl.Document, error)
}

// Observer counts upstream attempts by outcome (ok, rate_limited, timeout, error, canceled).
// It is implemented by *observability.Metrics.
type Observer interface {
	UpstreamCall(outcome string)
}

type noopObserver struct{}

func (noopObserver) UpstreamCall(string) {}

// Client implements Crawler on top of a Scraper.
type Client struct {
	sc          Scraper
	sem         chan struct{}
	timeout     time.Duration
	maxRetries  int
	country     string
	languages   []string
	noStore     bool
	backoffBase time.Duration
	maxBackoff  time.Duration
	log         *zap.SugaredLogger
	obs         Observer
	sleep       func(ctx context.Context, d time.Duration) error
}

// Option configures optional Client settings.
type Option func(*Client)

// WithTimeout sets the per-call timeout (default 30s).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithMaxConcurrency limits concurrent Firecrawl calls per pod (default 4).
func WithMaxConcurrency(n int) Option {
	return func(c *Client) { c.sem = make(chan struct{}, max(n, 1)) }
}

// WithMaxRetries sets the retries after the first attempt (default 2).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = max(n, 0) } }

// WithBackoff sets the base delay of the exponential backoff (default 500ms).
func WithBackoff(base time.Duration) Option { return func(c *Client) { c.backoffBase = base } }

// WithLocation makes Firecrawl fetch from the given country (ISO 3166 alpha-2) and ask for the
// given languages. Sites that redirect or localize by visitor geography (an EU store that sends
// US visitors to the US store, a footer that only links the imprint in the German version)
// otherwise answer with the wrong variant. Empty country disables it.
func WithLocation(country string, languages []string) Option {
	return func(c *Client) { c.country, c.languages = country, languages }
}

// WithStoreInCache controls whether Firecrawl may keep the scraped pages in its own result cache.
// Imprint pages contain personal data; with false Firecrawl does not retain them (data
// minimization). It does not change credit costs (cached results cost the same).
func WithStoreInCache(store bool) Option { return func(c *Client) { c.noStore = !store } }

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) Option { return func(c *Client) { c.log = l.Sugar() } }

// WithObserver sets the metrics sink.
func WithObserver(o Observer) Option { return func(c *Client) { c.obs = o } }

// WithSleep replaces the context-aware sleep (for tests).
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// New builds a Client around sc.
func New(sc Scraper, opts ...Option) *Client {
	c := &Client{
		sc:          sc,
		sem:         make(chan struct{}, 4),
		timeout:     30 * time.Second,
		maxRetries:  2,
		backoffBase: 500 * time.Millisecond,
		maxBackoff:  10 * time.Second,
		log:         zap.NewNop().Sugar(),
		obs:         noopObserver{},
		sleep:       sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewSDK builds the official Firecrawl client. SDK-level retries are disabled
// because this package retries (it can honor Retry-After); the transport
// captures the Retry-After header, which the SDK error types do not expose.
func NewSDK(apiKey, apiURL string) (*firecrawl.Client, error) {
	httpClient := &http.Client{Transport: retryAfterTransport{base: http.DefaultTransport}}
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if apiURL != "" {
		opts = append(opts, option.WithAPIURL(apiURL))
	}
	cl, err := firecrawl.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("crawler: create firecrawl client: %w", err)
	}
	return cl, nil
}

type freshKey struct{}

// WithFresh marks ctx so that Firecrawl calls made with it bypass Firecrawl's own result
// cache (maxAge 0). Used for refresh=true; otherwise Firecrawl may answer with a copy up to
// its default max age old, even though our cache was bypassed.
func WithFresh(ctx context.Context) context.Context { return context.WithValue(ctx, freshKey{}, true) }

// Scrape implements Crawler.
func (c *Client) Scrape(ctx context.Context, url string, req ScrapeRequest) (*Page, error) {
	opts := c.options(req)
	if fresh, _ := ctx.Value(freshKey{}).(bool); fresh {
		opts.MaxAge = firecrawl.Int64(0)
	}
	for attempt := 0; ; attempt++ {
		doc, retryAfter, err := c.attempt(ctx, url, opts, attempt+1, req)
		if err == nil {
			p := toPage(url, doc)
			observability.TraceFrom(ctx).Add("crawler", "page received", "url", p.URL, "status_code", p.StatusCode,
				"markdown_bytes", len(p.Markdown), "links", len(p.Links), "has_json", p.JSON != nil, "has_branding", p.Branding != nil)
			return p, nil
		}
		if !retryable(err) || attempt >= c.maxRetries || ctx.Err() != nil {
			return nil, err
		}
		delay := c.backoff(attempt)
		if retryAfter > delay {
			delay = min(retryAfter, c.callTimeout(req))
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < delay+c.callTimeout(req) {
			// A retry that the load deadline would cut short only burns credits and turns a
			// classified (cacheable) error into an uncached deadline error.
			return nil, err
		}
		c.log.Debugw("retrying firecrawl call", observability.Fields(ctx), "attempt", attempt+1, "delay", delay, "code", ErrorCode(err))
		if serr := c.sleep(ctx, delay); serr != nil {
			return nil, serr
		}
	}
}

// attempt performs one semaphore-guarded call.
func (c *Client) attempt(ctx context.Context, url string, opts *firecrawl.ScrapeOptions, n int, req ScrapeRequest) (*firecrawl.Document, time.Duration, error) {
	tr := observability.TraceFrom(ctx)
	queued := time.Now()
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		tr.Add("crawler", "gave up waiting for a free upstream slot", "url", url, "waited", time.Since(queued))
		return nil, 0, ctx.Err()
	}
	waited := time.Since(queued)
	callStart := time.Now()

	box := &retryAfterBox{}
	callCtx, cancel := context.WithTimeout(context.WithValue(ctx, retryAfterKey{}, box), c.callTimeout(req))
	defer cancel()

	doc, err := c.sc.Scrape(callCtx, url, opts)
	outcome, err := classify(ctx, err)
	c.obs.UpstreamCall(outcome)
	if outcome == "account_error" {
		c.log.Errorw("firecrawl refused the account: check API key, remaining credits and pending terms", "error", err)
	}
	tr.Add("crawler", "firecrawl call", "url", url, "attempt", n, "outcome", outcome, "duration", time.Since(callStart),
		"queue_wait", waited, "formats", describeRequest(req), "retry_after", box.get(), "error", err)
	return doc, box.get(), err
}

// siteErrorCode reports Firecrawl error codes that describe the target site (DNS, TLS, blocked,
// all engines failed, ...) rather than Firecrawl itself. SCRAPE_TIMEOUT is handled as a timeout.
func siteErrorCode(code string) bool {
	return strings.HasPrefix(code, "SCRAPE_") && code != "SCRAPE_TIMEOUT"
}

func (c *Client) callTimeout(req ScrapeRequest) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return c.timeout
}

func (c *Client) backoff(attempt int) time.Duration {
	d := min(c.backoffBase<<attempt, c.maxBackoff)
	return d/2 + rand.N(d/2+1) // equal jitter: [d/2, d]
}

func (c *Client) options(req ScrapeRequest) *firecrawl.ScrapeOptions {
	var formats []string
	if req.Markdown {
		formats = append(formats, "markdown")
	}
	if req.Links {
		formats = append(formats, "links")
	}
	if req.Branding {
		formats = append(formats, "branding")
	}
	opts := &firecrawl.ScrapeOptions{Formats: formats}
	if req.FullPage {
		opts.OnlyMainContent = firecrawl.Bool(false)
	}
	if req.WaitFor > 0 {
		opts.WaitFor = firecrawl.Int(int(req.WaitFor.Milliseconds()))
	}
	if req.JSON != nil {
		opts.JsonOptions = &firecrawl.JsonOptions{Prompt: req.JSON.Prompt, Schema: req.JSON.Schema}
	}
	if c.noStore {
		opts.StoreInCache = firecrawl.Bool(false)
	}
	if c.country != "" {
		opts.Location = &firecrawl.LocationConfig{Country: c.country, Languages: c.languages}
	}
	// Let Firecrawl give up slightly before our own per-call timeout fires.
	opts.Timeout = firecrawl.Int(max(int(c.callTimeout(req).Milliseconds())*9/10, 1000))
	return opts
}

// classify turns an SDK error into one of the typed errors and a metrics outcome.
// Cancellation of the caller's ctx is returned unchanged and is never an upstream failure.
func classify(parent context.Context, err error) (outcome string, _ error) {
	if err == nil {
		return "ok", nil
	}
	if parent.Err() != nil {
		return "canceled", parent.Err()
	}
	var rl *firecrawl.RateLimitError
	var auth *firecrawl.AuthenticationError
	var fe *firecrawl.FirecrawlError
	var netErr net.Error
	switch {
	case errors.As(err, &auth), errors.As(err, &fe) && (fe.StatusCode == http.StatusPaymentRequired || fe.RequiresAction != nil):
		return "account_error", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	case errors.As(err, &fe) && fe.StatusCode == http.StatusRequestTimeout:
		return "timeout", fmt.Errorf("%w: %w", ErrUpstreamTimeout, err) // Firecrawl's own scrape timeout
	case errors.As(err, &fe) && fe.ErrorCode == "SCRAPE_DNS_RESOLUTION_ERROR":
		return "error", fmt.Errorf("%w: %w: %w", ErrHostNotFound, ErrUpstream, err)
	case errors.As(err, &fe) && siteErrorCode(fe.ErrorCode):
		// Firecrawl names a problem of the target (SCRAPE_SSL_ERROR came with HTTP 500, seen live on
		// kern-ux.de with an expired certificate): a site failure, whatever the HTTP status.
		return "error", fmt.Errorf("%w: %w", ErrUpstream, err)
	case errors.As(err, &fe) && (fe.StatusCode >= 500 || fe.StatusCode == 0 && errors.Unwrap(fe) != nil):
		// Firecrawl failing or unreachable: an outage, not a property of the site.
		return "outage", fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	case errors.As(err, &rl):
		return "rate_limited", fmt.Errorf("%w: %w", ErrRateLimited, err)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout", fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
	default:
		return "error", fmt.Errorf("%w: %w", ErrUpstream, err)
	}
}

// retryable: 429, 409, 5xx and transport failures. Timeouts (including Firecrawl's 408)
// are not retried because the retry could not finish inside the load deadline anyway.
func retryable(err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	var fe *firecrawl.FirecrawlError
	if !errors.As(err, &fe) || fe.RequiresAction != nil || siteErrorCode(fe.ErrorCode) {
		return false
	}
	switch {
	case fe.StatusCode >= 500, fe.StatusCode == http.StatusConflict:
		return true
	case fe.StatusCode == 0:
		return errors.Unwrap(fe) != nil // transport error carries its cause
	}
	return false
}

func toPage(requested string, doc *firecrawl.Document) *Page {
	p := &Page{
		URL:      requested,
		Markdown: doc.Markdown,
		Links:    doc.Links,
		Branding: doc.Branding,
		Metadata: doc.Metadata,
	}
	if m, ok := doc.JSON.(map[string]any); ok {
		p.JSON = m
	}
	if u, ok := doc.Metadata["url"].(string); ok && u != "" {
		p.URL = u
	}
	switch v := doc.Metadata["statusCode"].(type) {
	case float64:
		p.StatusCode = int(v)
	case int:
		p.StatusCode = v
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- Retry-After capture -----------------------------------------------------

type retryAfterKey struct{}

type retryAfterBox struct{ ns atomic.Int64 }

func (b *retryAfterBox) get() time.Duration { return time.Duration(b.ns.Load()) }

// retryAfterTransport records the Retry-After header of 429/503 responses in
// the box carried by the request context.
type retryAfterTransport struct{ base http.RoundTripper }

func (t retryAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if box, ok := req.Context().Value(retryAfterKey{}).(*retryAfterBox); ok {
			box.ns.Store(int64(parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())))
		}
	}
	return resp, nil
}

// parseRetryAfter understands delta-seconds and HTTP dates; anything else is 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// describeRequest lists what a request asks for, for the debug trace.
func describeRequest(r ScrapeRequest) string {
	var f []string
	for _, x := range []struct {
		on   bool
		name string
	}{{r.Markdown, "markdown"}, {r.Links, "links"}, {r.Branding, "branding"}, {r.JSON != nil, "json"}, {r.FullPage, "full_page"}, {r.WaitFor > 0, "wait_for"}} {
		if x.on {
			f = append(f, x.name)
		}
	}
	return strings.Join(f, ",")
}
