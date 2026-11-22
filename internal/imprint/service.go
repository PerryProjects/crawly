package imprint

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/net/publicsuffix"

	"github.com/OWNER/siteinfo/internal/crawler"
	"github.com/OWNER/siteinfo/internal/observability"
)

// fallbackPaths are probed (at most these three) when the homepage links no imprint.
var fallbackPaths = []string{"/impressum", "/imprint", "/legal-notice"}

// maxLinkedAttempts bounds the ranked, linked candidates tried before the fallback paths.
const maxLinkedAttempts = 2

// imprintMarkers must appear in a genuine imprint page. They reject soft-404
// pages (a probe such as /impressum answered with the homepage).
var imprintMarkers = []string{
	"impressum", "imprint", "legal notice", "anbieterkennung", "offenlegung",
	"rechtliche hinweise", "angaben gemäß", "angaben gemaess", "medieninhaber", "mentions légales",
}

// Service discovers and extracts the imprint of a host.
type Service struct {
	scraper   Scraper
	extractor Extractor
	log       *zap.SugaredLogger
}

// Option configures optional Service settings.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) Option { return func(s *Service) { s.log = l.Sugar() } }

// NewService wires discovery (scraper) and extraction (extractor).
func NewService(scraper Scraper, extractor Extractor, opts ...Option) *Service {
	s := &Service{scraper: scraper, extractor: extractor, log: zap.NewNop().Sugar()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Lookup finds the imprint of host (normalized, no scheme). It returns
// ErrNotFound when no page qualifies; crawler errors are passed through.
//
// If the host itself has no imprint and is a subdomain (a shop on eu.store.example.com,
// whose legal pages live on example.com), the registrable parent domain is searched too.
// That costs extra upstream calls, so the parent is only searched through its homepage
// links, not through the well-known paths, and the result carries a warning.
func (s *Service) Lookup(ctx context.Context, host string) (*Imprint, error) {
	imp, err := s.lookupHost(ctx, host, nil, true)
	if !errors.Is(err, ErrNotFound) {
		return imp, err
	}
	parent, ok := parentDomain(host)
	if !ok {
		return nil, err
	}
	tr := observability.TraceFrom(ctx)
	tr.Add("imprint", "no imprint on this host, searching the parent domain", "parent", parent)
	imp, perr := s.lookupHost(ctx, parent, []string{host}, false)
	switch {
	case perr == nil:
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("imprint taken from the parent domain %s; %s has none of its own", parent, host))
		return imp, nil
	case errors.Is(perr, ErrNotFound), errors.Is(perr, crawler.ErrUpstream) && !hardFailure(perr):
		tr.Add("imprint", "parent domain has no imprint either", "parent", parent, "error", perr)
		return nil, err // the original not-found outcome stands
	default:
		return nil, perr // timeout, rate limit, cancellation
	}
}

// parentDomain returns the registrable domain of host (eTLD+1) when it differs from host.
// Private suffixes such as myshopify.com or blogspot.com count as public suffixes, so a
// shop hosted there never falls back to the hoster's own imprint.
func parentDomain(host string) (string, bool) {
	p, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || p == host {
		return "", false
	}
	return p, true
}

// lookupHost runs discovery on one host: homepage links first, then (if probes is set)
// the well-known paths. extraHosts are additional names considered the same website.
func (s *Service) lookupHost(ctx context.Context, host string, extraHosts []string, probes bool) (*Imprint, error) {
	homepage := "https://" + host + "/"

	home, err := s.scraper.Scrape(ctx, homepage, crawler.ScrapeRequest{Markdown: true, Links: true, FullPage: true})
	if err != nil {
		return nil, fmt.Errorf("imprint: scrape homepage: %w", err)
	}
	if home.StatusCode >= http.StatusBadRequest {
		// Firecrawl answers HTTP 200 with metadata.statusCode of the target (verified live for a 404).
		return nil, fmt.Errorf("%w: homepage returned HTTP %d", crawler.ErrUpstream, home.StatusCode)
	}
	base, err := url.Parse(home.URL)
	if err != nil || base.Hostname() == "" {
		base, _ = url.Parse(homepage)
	}
	hosts := append([]string{host, base.Hostname()}, extraHosts...)

	tr := observability.TraceFrom(ctx)
	links := MergeLinks(home.Links, home.Markdown)
	tr.Add("imprint", "homepage loaded", "final_url", home.URL, "status_code", home.StatusCode, "links_total", len(links))
	ranked := RankLinks(hosts, base, links)
	top := make([]map[string]any, 0, 8)
	for _, c := range ranked[:min(len(ranked), 8)] {
		top = append(top, map[string]any{"url": c.URL, "text": c.Text, "score": c.Score, "external": c.External})
	}
	tr.Add("imprint", "imprint link candidates (best first; the first two are tried)", "count", len(ranked), "top", top)
	attempts := ranked[:min(len(ranked), maxLinkedAttempts)]

	tried := map[string]bool{}
	try := func(c Candidate) (*Imprint, error) {
		tried[c.URL] = true
		x, err := s.extractor.Extract(ctx, c.URL)
		if err != nil {
			tr.Add("imprint", "candidate scrape failed", "url", c.URL, "abort_lookup", hardFailure(err), "error", err)
			if hardFailure(err) {
				return nil, err
			}
			s.log.Debugw("imprint candidate failed", observability.Fields(ctx), "url", c.URL, "error", err)
			return nil, nil
		}
		if reason := unusableReason(x); reason != "" {
			tr.Add("imprint", "candidate rejected", "url", c.URL, "reason", reason, "status_code", x.StatusCode, "text_bytes", len(x.Text))
			s.log.Debugw("imprint candidate rejected", observability.Fields(ctx), "url", c.URL, "status", x.StatusCode)
			return nil, nil
		}
		imp := build(x, c, hosts)
		tr.Add("imprint", "candidate accepted", "url", imp.ImprintURL, "text_bytes", len(x.Text), "warnings", imp.Warnings, "extraction_warnings", x.Warnings)
		return imp, nil
	}

	for _, c := range attempts {
		if imp, err := try(c); imp != nil || err != nil {
			return imp, err
		}
	}
	for _, p := range fallbackPaths {
		if !probes {
			break
		}
		c := Candidate{URL: "https://" + host + p}
		if tried[c.URL] {
			continue
		}
		tr.Add("imprint", "trying well-known path", "url", c.URL)
		if imp, err := try(c); imp != nil || err != nil {
			return imp, err
		}
	}
	tr.Add("imprint", "no imprint page found", "tried", len(tried))
	return nil, ErrNotFound
}

// hardFailure: errors that say nothing about the candidate page and must abort the lookup.
func hardFailure(err error) bool {
	return !errors.Is(err, crawler.ErrUpstream) || errors.Is(err, crawler.ErrUpstreamTimeout) || errors.Is(err, crawler.ErrRateLimited)
}

// unusableReason returns why a page is not an imprint, or "" if it qualifies: it must have
// returned a success status and read like an imprint.
func unusableReason(x Extraction) string {
	switch {
	case x.StatusCode >= 400:
		return fmt.Sprintf("target answered HTTP %d", x.StatusCode)
	case strings.TrimSpace(x.Text) == "":
		return "empty page"
	}
	lower := strings.ToLower(x.Text)
	if !slices.ContainsFunc(imprintMarkers, func(m string) bool { return strings.Contains(lower, m) }) {
		return "no imprint marker word in the text (soft 404?)"
	}
	return ""
}

func build(x Extraction, c Candidate, hosts []string) *Imprint {
	imp := &Imprint{ImprintURL: x.URL, Fields: x.Fields, Text: x.Text}
	if imp.ImprintURL == "" {
		imp.ImprintURL = c.URL
	}
	if imp.RepresentedBy == nil {
		imp.RepresentedBy = []string{}
	}
	imp.Warnings = append([]string{}, x.Warnings...)
	imp.Warnings = append(imp.Warnings, Validate(&imp.Fields, imp.Text)...)
	if u, err := url.Parse(imp.ImprintURL); err == nil && !related(u.Hostname(), hosts) {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("imprint page is hosted on a different host (%s)", u.Hostname()))
	}
	return imp
}
