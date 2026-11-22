package imprint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/OWNER/siteinfo/internal/crawler"
)

// fakeSite serves canned pages and records every scrape request.
type fakeSite struct {
	mu    sync.Mutex
	pages map[string]*crawler.Page
	errs  map[string]error
	calls []string
}

func (f *fakeSite) Scrape(_ context.Context, url string, _ crawler.ScrapeRequest) (*crawler.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	if err := f.errs[url]; err != nil {
		return nil, err
	}
	if p, ok := f.pages[url]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: HTTP 404", crawler.ErrUpstream)
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

func newSvc(site *fakeSite) *Service {
	return NewService(site, NewFirecrawlExtractor(site))
}

func TestLookupFollowsLinkedImprint(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/": {URL: "https://www.acme.de/", Markdown: "[Datenschutz](/datenschutz) [Impressum](/rechtliches/impressum)",
			Links: []string{"https://www.acme.de/datenschutz", "https://www.acme.de/rechtliches/impressum"}},
		"https://www.acme.de/rechtliches/impressum": imprintPage("https://www.acme.de/rechtliches/impressum"),
	}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if imp.ImprintURL != "https://www.acme.de/rechtliches/impressum" || imp.Text != imprintMD {
		t.Fatalf("url=%s text=%q", imp.ImprintURL, imp.Text)
	}
	if *imp.CompanyName != "ACME Beispiel GmbH" || *imp.VATID != "DE123456789" || *imp.Register.Type != "HRB" || *imp.Country != "DE" {
		t.Fatalf("fields = %+v", imp.Fields)
	}
	if len(imp.Warnings) != 0 || imp.Warnings == nil {
		t.Fatalf("warnings = %#v, want empty non-nil", imp.Warnings)
	}
	if len(site.calls) != 2 {
		t.Fatalf("calls = %v (privacy page must not be scraped)", site.calls)
	}
}

func TestLookupFallsBackToWellKnownPaths(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/":        {URL: "https://acme.de/", Markdown: "no links here"},
		"https://acme.de/imprint": imprintPage("https://acme.de/imprint"),
	}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if imp.ImprintURL != "https://acme.de/imprint" {
		t.Fatalf("url = %s", imp.ImprintURL)
	}
	want := []string{"https://acme.de/", "https://acme.de/impressum", "https://acme.de/imprint"}
	if !slices.Equal(site.calls, want) {
		t.Fatalf("calls = %v, want %v", site.calls, want)
	}
}

func TestLookupNotFoundTriesAtMostThreeFallbacks(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}}}
	_, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if len(site.calls) != 4 { // homepage + 3 well-known paths
		t.Fatalf("calls = %v", site.calls)
	}
}

func TestLookupSkipsFailedLinkedCandidateThenFallback(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/": {URL: "https://acme.de/", Links: []string{"https://acme.de/impressum-alt"}},
		// /impressum-alt is missing (404 -> ErrUpstream), /impressum works.
		"https://acme.de/impressum": imprintPage("https://acme.de/impressum"),
	}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil || imp.ImprintURL != "https://acme.de/impressum" {
		t.Fatalf("imp=%v err=%v calls=%v", imp, err, site.calls)
	}
}

func TestLookupRejectsSoftAndHardErrorPages(t *testing.T) {
	soft := &crawler.Page{URL: "https://acme.de/impressum", StatusCode: 200, Markdown: "# Willkommen\nUnsere Produkte"}
	notFound := &crawler.Page{URL: "https://acme.de/imprint", StatusCode: 404, Markdown: "Impressum 404"}
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/":          {URL: "https://acme.de/"},
		"https://acme.de/impressum": soft,
		"https://acme.de/imprint":   notFound,
	}}
	if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLookupExternalHostWarns(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/": {URL: "https://acme.de/", Links: []string{"https://legal-hoster.example/impressum/acme"}, Markdown: "[Impressum](https://legal-hoster.example/impressum/acme)"},
		"https://legal-hoster.example/impressum/acme": imprintPage("https://legal-hoster.example/impressum/acme"),
	}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(imp.Warnings, "different host (legal-hoster.example)") {
		t.Fatalf("warnings = %v", imp.Warnings)
	}
}

func TestLookupErrorPropagation(t *testing.T) {
	t.Run("homepage failure", func(t *testing.T) {
		site := &fakeSite{errs: map[string]error{"https://acme.de/": crawler.ErrUpstream}}
		if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, crawler.ErrUpstream) {
			t.Fatalf("err = %v", err)
		}
	})
	for name, cause := range map[string]error{"timeout": crawler.ErrUpstreamTimeout, "rate limit": crawler.ErrRateLimited, "canceled": context.Canceled} {
		t.Run("candidate "+name+" aborts the lookup", func(t *testing.T) {
			site := &fakeSite{
				pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}},
				errs:  map[string]error{"https://acme.de/impressum": cause},
			}
			_, err := newSvc(site).Lookup(context.Background(), "acme.de")
			if !errors.Is(err, cause) || errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v", err)
			}
			if len(site.calls) != 2 {
				t.Fatalf("must stop at the failing candidate, calls = %v", site.calls)
			}
		})
	}
}

func TestLookupKeepsTextWhenStructuredExtractionFails(t *testing.T) {
	page := imprintPage("https://acme.de/impressum")
	page.JSON = nil
	site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": page}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if imp.Text != imprintMD || imp.CompanyName != nil || !hasWarning(imp.Warnings, "structured extraction returned no data") {
		t.Fatalf("imp = %+v", imp)
	}
}

func TestLookupWarnsAboutHallucinatedValues(t *testing.T) {
	page := imprintPage("https://acme.de/impressum")
	page.JSON["vat_id"] = "DE999999999"
	site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": page}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil || !hasWarning(imp.Warnings, "vat_id not found verbatim") {
		t.Fatalf("err=%v warnings=%v", err, imp.Warnings)
	}
	if !strings.Contains(imp.Text, "DE123456789") {
		t.Fatal("raw text must stay untouched")
	}
}
