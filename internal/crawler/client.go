package crawler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/firecrawl/firecrawl/apps/go-sdk"
	"github.com/firecrawl/firecrawl/apps/go-sdk/option"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/observability"
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
	backoffBase time.Duration
	maxBackoff  time.Duration
	log         *zap.Logger
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

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) Option { return func(c *Client) { c.log = l } }

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
		log:         zap.NewNop(),
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

// Scrape implements Crawler.
func (c *Client) Scrape(ctx context.Context, url string, req ScrapeRequest) (*Page, error) {
	opts := c.options(req)
	for attempt := 0; ; attempt++ {
		doc, retryAfter, err := c.attempt(ctx, url, opts)
		if err == nil {
			return toPage(url, doc), nil
		}
		if !retryable(err) || attempt >= c.maxRetries || ctx.Err() != nil {
			return nil, err
		}
		delay := c.backoff(attempt)
		if retryAfter > delay {
			delay = min(retryAfter, c.timeout)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= delay {
			return nil, err // not enough budget left to wait and retry
		}
		c.log.Debug("retrying firecrawl call", observability.Fields(ctx), zap.Int("attempt", attempt+1), zap.Duration("delay", delay), zap.String("code", ErrorCode(err)))
		if serr := c.sleep(ctx, delay); serr != nil {
			return nil, serr
		}
	}
}

// attempt performs one semaphore-guarded call.
func (c *Client) attempt(ctx context.Context, url string, opts *firecrawl.ScrapeOptions) (*firecrawl.Document, time.Duration, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	box := &retryAfterBox{}
	callCtx, cancel := context.WithTimeout(context.WithValue(ctx, retryAfterKey{}, box), c.timeout)
	defer cancel()

	doc, err := c.sc.Scrape(callCtx, url, opts)
	outcome, err := classify(ctx, err)
	c.obs.UpstreamCall(outcome)
	return doc, box.get(), err
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
	if req.JSON != nil {
		opts.JsonOptions = &firecrawl.JsonOptions{Prompt: req.JSON.Prompt, Schema: req.JSON.Schema}
	}
	// Let Firecrawl give up slightly before our own per-call timeout fires.
	opts.Timeout = firecrawl.Int(max(int(c.timeout.Milliseconds())*9/10, 1000))
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
	var netErr net.Error
	switch {
	case errors.As(err, &rl):
		return "rate_limited", fmt.Errorf("%w: %w", ErrRateLimited, err)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout", fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
	default:
		return "error", fmt.Errorf("%w: %w", ErrUpstream, err)
	}
}

// retryable: 429, 408/409, 5xx and transport failures. Timeouts are not retried
// because the retry could not finish inside the load deadline anyway.
func retryable(err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	if !errors.Is(err, ErrUpstream) {
		return false
	}
	var fe *firecrawl.FirecrawlError
	if !errors.As(err, &fe) {
		return false
	}
	switch {
	case fe.StatusCode >= 500, fe.StatusCode == http.StatusRequestTimeout, fe.StatusCode == http.StatusConflict:
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
