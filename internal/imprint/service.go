package imprint

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"go.uber.org/zap"

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
	log       *zap.Logger
}

// Option configures optional Service settings.
type Option func(*Service)

// WithLogger sets the fallback logger.
func WithLogger(l *zap.Logger) Option { return func(s *Service) { s.log = l } }

// NewService wires discovery (scraper) and extraction (extractor).
func NewService(scraper Scraper, extractor Extractor, opts ...Option) *Service {
	s := &Service{scraper: scraper, extractor: extractor, log: zap.NewNop()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Lookup finds the imprint of host (normalized, no scheme). It returns
// ErrNotFound when no page qualifies; crawler errors are passed through.
func (s *Service) Lookup(ctx context.Context, host string) (*Imprint, error) {
	log := observability.LoggerFrom(ctx, s.log).With(zap.String("host", host))
	homepage := "https://" + host + "/"

	home, err := s.scraper.Scrape(ctx, homepage, crawler.ScrapeRequest{Markdown: true, Links: true, FullPage: true})
	if err != nil {
		return nil, fmt.Errorf("imprint: scrape homepage: %w", err)
	}
	base, err := url.Parse(home.URL)
	if err != nil || base.Hostname() == "" {
		base, _ = url.Parse(homepage)
	}
	hosts := []string{host, base.Hostname()}

	ranked := RankLinks(hosts, base, MergeLinks(home.Links, home.Markdown))
	var attempts []Candidate
	for _, c := range ranked[:min(len(ranked), maxLinkedAttempts)] {
		attempts = append(attempts, c)
	}

	tried := map[string]bool{}
	try := func(c Candidate) (*Imprint, error) {
		tried[c.URL] = true
		x, err := s.extractor.Extract(ctx, c.URL)
		if err != nil {
			if hardFailure(err) {
				return nil, err
			}
			log.Debug("imprint candidate failed", zap.String("url", c.URL), zap.Error(err))
			return nil, nil
		}
		if !usable(x) {
			log.Debug("imprint candidate rejected", zap.String("url", c.URL), zap.Int("status", x.StatusCode))
			return nil, nil
		}
		return build(x, c, hosts), nil
	}

	for _, c := range attempts {
		if imp, err := try(c); imp != nil || err != nil {
			return imp, err
		}
	}
	for _, p := range fallbackPaths {
		c := Candidate{URL: "https://" + host + p}
		if tried[c.URL] {
			continue
		}
		if imp, err := try(c); imp != nil || err != nil {
			return imp, err
		}
	}
	return nil, ErrNotFound
}

// hardFailure: errors that say nothing about the candidate page and must abort the lookup.
func hardFailure(err error) bool {
	return !errors.Is(err, crawler.ErrUpstream) || errors.Is(err, crawler.ErrUpstreamTimeout) || errors.Is(err, crawler.ErrRateLimited)
}

// usable accepts a page that returned a success status and reads like an imprint.
func usable(x Extraction) bool {
	if x.StatusCode >= 400 || strings.TrimSpace(x.Text) == "" {
		return false
	}
	lower := strings.ToLower(x.Text)
	return slices.ContainsFunc(imprintMarkers, func(m string) bool { return strings.Contains(lower, m) })
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
