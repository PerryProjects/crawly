// Package crawler wraps the Firecrawl Go SDK behind a small interface and adds
// a per-pod concurrency limit, per-call timeouts and retries with backoff.
package crawler

import (
	"context"
	"errors"
	"time"
)

// Typed upstream errors. Callers match them with errors.Is.
var (
	// ErrUpstream is any failed Firecrawl call that is not a timeout or rate limit.
	ErrUpstream = errors.New("crawler: upstream error")
	// ErrUpstreamTimeout means the per-call timeout expired.
	ErrUpstreamTimeout = errors.New("crawler: upstream timeout")
	// ErrRateLimited means Firecrawl answered HTTP 429 and retries were exhausted.
	ErrRateLimited = errors.New("crawler: upstream rate limited")
	// ErrUpstreamUnavailable means Firecrawl itself could not serve us: it refused our account
	// (invalid API key 401, no credits 402, pending terms) or it is failing (5xx, unreachable).
	// It says nothing about the website, so it must never be cached as a per-site failure.
	ErrUpstreamUnavailable = errors.New("crawler: upstream unavailable")
	// ErrHostNotFound means the site's domain does not resolve (DNS). It also matches ErrUpstream
	// (a site failure that is negative-cached), but callers can tell the user to check the URL.
	ErrHostNotFound = errors.New("crawler: host not found")
)

// Stable error codes, also used as error_code in cached failure envelopes.
const (
	CodeUpstream     = "upstream_error"
	CodeTimeout      = "upstream_timeout"
	CodeRateLimited  = "upstream_rate_limited"
	CodeUnavailable  = "upstream_unavailable"
	CodeHostNotFound = "host_not_found"
)

// ErrorCode returns the stable code for a crawler error, or "" for any other error.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrHostNotFound):
		return CodeHostNotFound
	case errors.Is(err, ErrUpstreamTimeout):
		return CodeTimeout
	case errors.Is(err, ErrRateLimited):
		return CodeRateLimited
	case errors.Is(err, ErrUpstreamUnavailable):
		return CodeUnavailable
	case errors.Is(err, ErrUpstream):
		return CodeUpstream
	}
	return ""
}

// JSONExtract asks Firecrawl to run an LLM extraction against a JSON schema.
type JSONExtract struct {
	Prompt string
	Schema map[string]any
}

// ScrapeRequest selects what to fetch for one URL.
type ScrapeRequest struct {
	Markdown bool
	Links    bool
	Branding bool
	// FullPage disables Firecrawl's "main content only" filter. Needed for
	// footer links and for legally relevant page text.
	FullPage bool
	// WaitFor lets the page's JavaScript run before the content is taken (0 = Firecrawl default).
	WaitFor time.Duration
	// Timeout overrides the client's per-call timeout for this request (0 = default).
	Timeout time.Duration
	JSON    *JSONExtract
}

// Page is the subset of a Firecrawl document this service uses.
type Page struct {
	URL        string
	StatusCode int // target site status from metadata, 0 if unknown
	Markdown   string
	Links      []string
	JSON       map[string]any // LLM extraction result, nil if absent
	Branding   map[string]any // untyped branding payload, nil if absent
	Metadata   map[string]any
}

// Crawler fetches one page. The Firecrawl implementation is *Client; tests use fakes.
type Crawler interface {
	Scrape(ctx context.Context, url string, req ScrapeRequest) (*Page, error)
}
