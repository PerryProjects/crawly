package imprint

import (
	"context"
	"fmt"
	"sync"

	"github.com/perryprojects/siteinfo/internal/crawler"
)

// mockSite is a Scraper mock: it serves canned pages and a canned site map and records every
// call, so tests can assert what was fetched and how many Firecrawl credits that would cost.
// No test in this package talks to Firecrawl.
type mockSite struct {
	mu         sync.Mutex
	pages      map[string]*crawler.Page
	errs       map[string]error
	unrendered map[string]*crawler.Page // served instead of pages when the request does not wait for JavaScript
	sitemap    []crawler.MapLink
	mapErr     error
	jsonErr    error // returned for every request that asks for the LLM extraction

	scrapes []scrapeCall
	maps    int
}

type scrapeCall struct {
	URL string
	Req crawler.ScrapeRequest
}

func (m *mockSite) Scrape(_ context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scrapes = append(m.scrapes, scrapeCall{URL: url, Req: req})
	if err := m.errs[url]; err != nil {
		return nil, err
	}
	if req.JSON != nil && m.jsonErr != nil {
		return nil, m.jsonErr
	}
	if p, ok := m.unrendered[url]; ok && req.WaitFor == 0 {
		return p, nil
	}
	if p, ok := m.pages[url]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: HTTP 404", crawler.ErrUpstream)
}

func (m *mockSite) Map(context.Context, string, string, int) ([]crawler.MapLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maps++
	return m.sitemap, m.mapErr
}

// urls lists the scraped URLs in call order.
func (m *mockSite) urls() []string {
	out := make([]string, len(m.scrapes))
	for i, c := range m.scrapes {
		out[i] = c.URL
	}
	return out
}

// credits estimates the Firecrawl cost of the recorded calls: 1 per scrape, +4 for the LLM
// extraction, 1 per map call (Firecrawl billing docs).
func (m *mockSite) credits() int {
	n := m.maps
	for _, c := range m.scrapes {
		n++
		if c.Req.JSON != nil {
			n += 4
		}
	}
	return n
}

const imprintMD = "# Impressum\n\nACME Beispiel GmbH\nMusterstraße 1, 10115 Berlin\ninfo@acme.de\nUSt-IdNr.: DE123456789\nHRB 12345 B"

func imprintPage(url string) *crawler.Page {
	return &crawler.Page{URL: url, StatusCode: 200, Markdown: imprintMD, JSON: map[string]any{
		"company_name": "ACME Beispiel GmbH", "legal_form": "GmbH", "represented_by": []any{"Max Mustermann"},
		"address":  map[string]any{"street": "Musterstraße 1", "postal_code": "10115", "city": "Berlin", "country": "Deutschland"},
		"contact":  map[string]any{"email": "info@acme.de"},
		"register": map[string]any{"type": "hrb", "court": "Amtsgericht Berlin", "number": "12345 B"},
		"vat_id":   "DE 123 456 789", "country": "DE",
	}}
}

func homepage(url string, links ...string) *crawler.Page {
	md := ""
	for _, l := range links {
		md += "[Impressum](" + l + ") "
	}
	return &crawler.Page{URL: url, StatusCode: 200, Markdown: md, Links: links}
}

func newSvc(site *mockSite) *Service { return NewService(site, NewFirecrawlExtractor(site)) }
