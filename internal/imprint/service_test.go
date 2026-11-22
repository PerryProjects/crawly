package imprint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/perryprojects/siteinfo/internal/crawler"
)

// fakeSite serves canned pages and records every scrape request.
type fakeSite struct {
	mu    sync.Mutex
	pages map[string]*crawler.Page
	errs  map[string]error
	calls []string
	reqs  []crawler.ScrapeRequest
}

func (f *fakeSite) Scrape(_ context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	f.reqs = append(f.reqs, req)
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
	// homepage, then the imprint page twice (markdown probe, then JSON extraction); never the privacy page.
	if want := []string{"https://acme.de/", "https://www.acme.de/rechtliches/impressum", "https://www.acme.de/rechtliches/impressum"}; !slices.Equal(site.calls, want) {
		t.Fatalf("calls = %v", site.calls)
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
	want := []string{"https://acme.de/", "https://acme.de/impressum", "https://acme.de/imprint", "https://acme.de/imprint"}
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
	for name, cause := range map[string]error{"timeout": crawler.ErrUpstreamTimeout, "rate limit": crawler.ErrRateLimited, "account": crawler.ErrUpstreamUnavailable, "canceled": context.Canceled} {
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

func TestLookupHomepageErrorStatusIsUpstreamError(t *testing.T) {
	for _, status := range []int{403, 404, 500, 503} {
		site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/", StatusCode: status, Markdown: "[Impressum](/impressum)"}}}
		_, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if !errors.Is(err, crawler.ErrUpstream) || errors.Is(err, ErrNotFound) || len(site.calls) != 2 { // apex, then www
			t.Fatalf("status %d: err=%v calls=%v", status, err, site.calls)
		}
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
	if err != nil || !hasWarning(imp.Warnings, "vat_id not found verbatim") || imp.VATID != nil {
		t.Fatalf("err=%v vat=%v warnings=%v", err, imp.VATID, imp.Warnings)
	}
	if !strings.Contains(imp.Text, "DE123456789") {
		t.Fatal("raw text must stay untouched")
	}
}

func TestParentDomain(t *testing.T) {
	for host, want := range map[string]string{
		"eu.store.bambulab.com":         "bambulab.com",
		"shop.example.de":               "example.de",
		"a.b.c.example.co.uk":           "example.co.uk",
		"example.de":                    "",
		"example.co.uk":                 "",
		"shop.myshopify.com":            "", // private suffix: never fall back to the hoster
		"user.blogspot.com":             "",
		"xn--bcher-kva.shop.example.at": "example.at",
	} {
		got, ok := parentDomain(host)
		if got != want || ok != (want != "") {
			t.Errorf("parentDomain(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestLookupFallsBackToParentDomain(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://eu.store.acme.de/":                     {URL: "https://eu.store.acme.de/", Markdown: "store without legal links"},
		"https://acme.de/":                              {URL: "https://acme.de/de-de", Markdown: "[Impressum](https://acme.de/de-de/policies/page/impressum)", Links: []string{"https://acme.de/de-de/policies/page/impressum"}},
		"https://acme.de/de-de/policies/page/impressum": imprintPage("https://acme.de/de-de/policies/page/impressum"),
	}}
	imp, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if imp.ImprintURL != "https://acme.de/de-de/policies/page/impressum" {
		t.Fatalf("url = %s", imp.ImprintURL)
	}
	if !hasWarning(imp.Warnings, "taken from the parent domain acme.de") || hasWarning(imp.Warnings, "different host") {
		t.Fatalf("warnings = %v", imp.Warnings)
	}
	want := []string{
		"https://eu.store.acme.de/", "https://eu.store.acme.de/impressum", "https://eu.store.acme.de/imprint", "https://eu.store.acme.de/legal-notice",
		"https://acme.de/", "https://acme.de/de-de/policies/page/impressum", "https://acme.de/de-de/policies/page/impressum",
	}
	if !slices.Equal(site.calls, want) {
		t.Fatalf("calls = %v\nwant    %v", site.calls, want)
	}
}

func TestParentDomainDoesNotProbeWellKnownPaths(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://eu.store.acme.de/": {URL: "https://eu.store.acme.de/"},
		"https://acme.de/":          {URL: "https://acme.de/", Markdown: "nothing relevant"},
		"https://acme.de/impressum": imprintPage("https://acme.de/impressum"), // exists, but is not probed
	}}
	if _, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if len(site.calls) != 5 { // host homepage + 3 probes + parent homepage
		t.Fatalf("calls = %v", site.calls)
	}
}

func TestParentDomainFailuresKeepNotFound(t *testing.T) {
	base := func() *fakeSite {
		return &fakeSite{pages: map[string]*crawler.Page{"https://eu.store.acme.de/": {URL: "https://eu.store.acme.de/"}}, errs: map[string]error{}}
	}
	t.Run("parent site down is still not found", func(t *testing.T) {
		s := base()
		s.errs["https://acme.de/"] = crawler.ErrUpstream
		if _, err := newSvc(s).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	for name, cause := range map[string]error{"timeout": crawler.ErrUpstreamTimeout, "rate limit": crawler.ErrRateLimited, "canceled": context.Canceled} {
		t.Run("parent "+name+" aborts", func(t *testing.T) {
			s := base()
			s.errs["https://acme.de/"] = cause
			if _, err := newSvc(s).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, cause) || errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestNoParentFallbackForHostedSubdomains(t *testing.T) {
	for _, host := range []string{"acme.de", "peters-shop.myshopify.com", "acme.blogspot.com"} {
		site := &fakeSite{pages: map[string]*crawler.Page{"https://" + host + "/": {URL: "https://" + host + "/"}}}
		if _, err := newSvc(site).Lookup(context.Background(), host); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v", host, err)
		}
		if len(site.calls) != 4 {
			t.Errorf("%s: calls = %v, want homepage plus 3 probes only", host, site.calls)
		}
	}
}

// The expensive LLM extraction must only run for pages that passed the cheap markdown check.
func TestExtractionOnlyRunsForAcceptedPages(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/":             {URL: "https://acme.de/"},
		"https://acme.de/impressum":    {URL: "https://acme.de/impressum", StatusCode: 404, Markdown: "Impressum 404"},
		"https://acme.de/imprint":      {URL: "https://acme.de/imprint", StatusCode: 200, Markdown: "# Willkommen\nkeine Rechtstexte"}, // soft 404
		"https://acme.de/legal-notice": imprintPage("https://acme.de/legal-notice"),
	}}
	if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); err != nil {
		t.Fatal(err)
	}
	jsonCalls := map[string]int{}
	for i, req := range site.reqs {
		if req.JSON != nil {
			jsonCalls[site.calls[i]]++
			if req.Markdown {
				t.Errorf("the extraction call does not need the markdown again: %+v", req)
			}
		}
	}
	if len(jsonCalls) != 1 || jsonCalls["https://acme.de/legal-notice"] != 1 {
		t.Fatalf("JSON extraction ran for %v, want only the accepted page, once", jsonCalls)
	}
	// 404 and soft 404 cost exactly one (markdown) call each.
	count := map[string]int{}
	for _, u := range site.calls {
		count[u]++
	}
	if count["https://acme.de/impressum"] != 1 || count["https://acme.de/imprint"] != 1 || count["https://acme.de/legal-notice"] != 2 {
		t.Fatalf("calls per URL = %v", count)
	}
}

func TestImprintPartialWhenExtractionFails(t *testing.T) {
	page := imprintPage("https://acme.de/impressum")
	page.JSON = nil
	site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": page}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil || !imp.PartialResult() {
		t.Fatalf("partial = %v, err = %v", imp.PartialResult(), err)
	}
	ok := imprintPage("https://acme.de/impressum")
	site = &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": ok}}
	if imp, _ = newSvc(site).Lookup(context.Background(), "acme.de"); imp.PartialResult() {
		t.Fatal("a complete result is not partial")
	}
}

func TestHugeTextIsTruncated(t *testing.T) {
	page := imprintPage("https://acme.de/impressum")
	page.Markdown = imprintMD + "\n" + strings.Repeat("ä", maxTextBytes) // multi-byte runes across the cut
	site := &fakeSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": page}}
	imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Text) > maxTextBytes || !utf8.ValidString(imp.Text) || !hasWarning(imp.Warnings, WarnTextTruncated) {
		t.Fatalf("len=%d valid=%v warnings=%+v", len(imp.Text), utf8.ValidString(imp.Text), imp.Warnings)
	}
	if imp.VATID == nil {
		t.Error("validation must still see the full text")
	}
}

func TestSoftNotFoundWithImprintFooterIsRejected(t *testing.T) {
	soft := "# Seite nicht gefunden\n\nFooter: [Impressum](https://acme.de/impressum) | [Datenschutz](/datenschutz)"
	if reason := unusableReason(Extraction{StatusCode: 200, Text: soft}); reason == "" {
		t.Fatal("a 404 page whose only 'Impressum' is a footer link must be rejected")
	}
	if reason := unusableReason(Extraction{StatusCode: 200, Text: imprintMD}); reason != "" {
		t.Fatalf("a real imprint was rejected: %s", reason)
	}
}

// After the markdown check accepted the page, a failed LLM call must not lose the imprint.
type failingJSONSite struct{ *fakeSite }

func (f failingJSONSite) Scrape(ctx context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error) {
	if req.JSON != nil {
		f.mu.Lock()
		f.calls = append(f.calls, url)
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: HTTP 200 [SCRAPE_ALL_ENGINES_FAILED]", crawler.ErrUpstream)
	}
	return f.fakeSite.Scrape(ctx, url, req)
}

func TestExtractionFailureKeepsAcceptedPage(t *testing.T) {
	site := failingJSONSite{&fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/": {URL: "https://acme.de/"}, "https://acme.de/impressum": imprintPage("https://acme.de/impressum"),
	}}}
	imp, err := NewService(site, NewFirecrawlExtractor(site)).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatalf("err = %v, want the page with a partial warning", err)
	}
	if imp.Text != imprintMD || !imp.PartialResult() || imp.CompanyName != nil {
		t.Fatalf("imp = %+v", imp)
	}
}

func TestEnglishImprintWithoutHeadingIsAccepted(t *testing.T) {
	text := "[Imprint](/pages/imprint)\n\ngitti GmbH, Kastanienallee 43a, 10435 Berlin\nRepresented by Founder & Managing Director: J. Doe\nRegister court: Charlottenburg Local Court"
	if reason := unusableReason(Extraction{StatusCode: 200, Text: text}); reason != "" {
		t.Fatalf("rejected: %s", reason)
	}
}

// renderingSite serves the imprint content only when the request waits for JavaScript.
type renderingSite struct{ *fakeSite }

func (r renderingSite) Scrape(ctx context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error) {
	p, err := r.fakeSite.Scrape(ctx, url, req)
	if err == nil && url == "https://www.acme.de/impressum" && req.WaitFor == 0 {
		return &crawler.Page{URL: p.URL, StatusCode: 200, Markdown: "[Impressum](https://www.acme.de/impressum) [Home](/)"}, nil
	}
	return p, err
}

func TestLinkedJavaScriptPageIsRetriedWithRenderWait(t *testing.T) {
	inner := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/":              {URL: "https://www.acme.de/", Links: []string{"https://www.acme.de/impressum"}, Markdown: "[Impressum](https://www.acme.de/impressum)"},
		"https://www.acme.de/impressum": imprintPage("https://www.acme.de/impressum"),
	}}
	site := renderingSite{inner}
	imp, err := NewService(site, NewFirecrawlExtractor(site)).Lookup(context.Background(), "acme.de")
	if err != nil || imp.CompanyName == nil {
		t.Fatalf("imp=%+v err=%v calls=%v", imp, err, inner.calls)
	}
	var waited int
	for _, r := range inner.reqs {
		if r.WaitFor > 0 {
			waited++
		}
	}
	if waited != 2 { // markdown retry and the JSON extraction both wait
		t.Fatalf("requests with a render wait = %d, calls = %v", waited, inner.calls)
	}
}

func TestProbesSkipURLsAlreadyFetched(t *testing.T) {
	site := &fakeSite{pages: map[string]*crawler.Page{
		"https://acme.de/":              {URL: "https://www.acme.de/", Links: []string{"https://www.acme.de/impressum"}},
		"https://www.acme.de/impressum": {URL: "https://www.acme.de/impressum", StatusCode: 404},
	}}
	if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, c := range site.calls {
		if c == "https://acme.de/impressum" {
			t.Fatalf("the www variant was already tried, calls = %v", site.calls)
		}
	}
}

func TestCanonicalImprintURL(t *testing.T) {
	for _, tt := range [][3]string{
		{"https://www.dfg.de/impressum?enodia=eyJ.abc", "https://www.dfg.de/impressum", "https://www.dfg.de/impressum"},
		{"https://acme.de/impressum?lang=de", "https://acme.de/impressum?lang=de", "https://acme.de/impressum?lang=de"},
		{"https://acme.de/legal/impressum", "https://acme.de/impressum", "https://acme.de/legal/impressum"}, // real redirect
		{"", "https://acme.de/impressum", "https://acme.de/impressum"},
	} {
		if got := canonicalImprintURL(tt[0], tt[1]); got != tt[2] {
			t.Errorf("canonicalImprintURL(%q, %q) = %q, want %q", tt[0], tt[1], got, tt[2])
		}
	}
}
