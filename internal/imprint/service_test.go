package imprint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/perryprojects/siteinfo/internal/crawler"
)

func TestLookupDiscovery(t *testing.T) {
	t.Run("strong same-host link is fetched with markdown and extraction in one call", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{
			"https://acme.de/": {URL: "https://www.acme.de/", Markdown: "[Datenschutz](/datenschutz) [Impressum](/rechtliches/impressum)",
				Links: []string{"https://www.acme.de/datenschutz", "https://www.acme.de/rechtliches/impressum"}},
			"https://www.acme.de/rechtliches/impressum": imprintPage("https://www.acme.de/rechtliches/impressum"),
		}}
		imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if err != nil {
			t.Fatal(err)
		}
		if imp.ImprintURL != "https://www.acme.de/rechtliches/impressum" || imp.Text != imprintMD || *imp.VATID != "DE123456789" {
			t.Fatalf("imprint = %+v", imp)
		}
		if want := []string{"https://acme.de/", "https://www.acme.de/rechtliches/impressum"}; !slices.Equal(site.urls(), want) {
			t.Fatalf("calls = %v, want %v (never the privacy page)", site.urls(), want)
		}
		if c := site.credits(); c != 6 { // homepage 1 + combined call 5
			t.Fatalf("credits = %d, want 6", c)
		}
		if len(imp.Warnings) != 0 || imp.Warnings == nil {
			t.Fatalf("warnings = %#v", imp.Warnings)
		}
	})

	t.Run("link matched only by its text is checked cheaply before extraction", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{
			"https://acme.de/":        {URL: "https://acme.de/", Markdown: "[Impressum](https://acme.de/info/42)", Links: []string{"https://acme.de/info/42"}},
			"https://acme.de/info/42": imprintPage("https://acme.de/info/42"),
		}}
		if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); err != nil {
			t.Fatal(err)
		}
		if len(site.scrapes) != 3 || site.scrapes[1].Req.JSON != nil || site.scrapes[2].Req.JSON == nil {
			t.Fatalf("want homepage, markdown check, extraction; got %v", site.urls())
		}
	})

	t.Run("no linked candidate: the site map finds a shop path", func(t *testing.T) {
		site := &mockSite{
			pages: map[string]*crawler.Page{
				"https://acme.de/":              homepage("https://acme.de/"),
				"https://acme.de/pages/imprint": imprintPage("https://acme.de/pages/imprint"),
			},
			sitemap: []crawler.MapLink{{URL: "https://acme.de/produkte"}, {URL: "https://acme.de/pages/imprint", Title: "Imprint"}},
		}
		imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if err != nil || imp.ImprintURL != "https://acme.de/pages/imprint" {
			t.Fatalf("imp=%v err=%v calls=%v", imp, err, site.urls())
		}
		if site.maps != 1 {
			t.Fatalf("map calls = %d", site.maps)
		}
	})

	t.Run("nothing linked or mapped: one guess, then not found", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/")}}
		if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		if want := []string{"https://acme.de/", "https://acme.de/impressum"}; !slices.Equal(site.urls(), want) || site.maps != 1 {
			t.Fatalf("calls = %v, maps = %d", site.urls(), site.maps)
		}
		if c := site.credits(); c != 3 { // homepage + map + one guess (the old three-path probing cost 4)
			t.Fatalf("credits = %d, want 3", c)
		}
	})

	t.Run("a failing site map falls back to the guess", func(t *testing.T) {
		site := &mockSite{
			pages:  map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/"), "https://acme.de/impressum": imprintPage("https://acme.de/impressum")},
			mapErr: fmt.Errorf("%w: map failed", crawler.ErrUpstream),
		}
		if imp, err := newSvc(site).Lookup(context.Background(), "acme.de"); err != nil || imp.ImprintURL != "https://acme.de/impressum" {
			t.Fatalf("imp=%v err=%v", imp, err)
		}
	})

	t.Run("failed linked candidate, then the site map", func(t *testing.T) {
		site := &mockSite{
			pages: map[string]*crawler.Page{
				"https://acme.de/":          homepage("https://acme.de/", "https://acme.de/impressum-alt"),
				"https://acme.de/impressum": imprintPage("https://acme.de/impressum"),
			},
			sitemap: []crawler.MapLink{{URL: "https://acme.de/impressum"}},
		}
		if imp, err := newSvc(site).Lookup(context.Background(), "acme.de"); err != nil || imp.ImprintURL != "https://acme.de/impressum" {
			t.Fatalf("imp=%v err=%v calls=%v", imp, err, site.urls())
		}
	})

	t.Run("URLs already fetched are not fetched again", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{
			"https://acme.de/":              homepage("https://www.acme.de/", "https://www.acme.de/impressum"),
			"https://www.acme.de/impressum": {URL: "https://www.acme.de/impressum", StatusCode: 404},
		}, sitemap: []crawler.MapLink{{URL: "https://acme.de/impressum"}}}
		if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if slices.Contains(site.urls(), "https://acme.de/impressum") {
			t.Fatalf("the www variant was already tried, calls = %v", site.urls())
		}
	})

	t.Run("linked page rendered by JavaScript is retried with a render wait", func(t *testing.T) {
		site := &mockSite{
			pages: map[string]*crawler.Page{
				"https://acme.de/":              homepage("https://www.acme.de/", "https://www.acme.de/impressum"),
				"https://www.acme.de/impressum": imprintPage("https://www.acme.de/impressum"),
			},
			unrendered: map[string]*crawler.Page{"https://www.acme.de/impressum": {URL: "https://www.acme.de/impressum", StatusCode: 200, Markdown: "[Impressum](/impressum) [Home](/)"}},
		}
		imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if err != nil || imp.CompanyName == nil {
			t.Fatalf("imp=%+v err=%v calls=%v", imp, err, site.urls())
		}
		var waited int
		for _, c := range site.scrapes {
			if c.Req.WaitFor > 0 {
				waited++
			}
		}
		if waited != 2 { // markdown retry and the extraction both wait
			t.Fatalf("requests with a render wait = %d", waited)
		}
	})

	t.Run("an external imprint is used but flagged", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{
			"https://acme.de/": homepage("https://acme.de/", "https://legal-hoster.example/impressum/acme"),
			"https://legal-hoster.example/impressum/acme": imprintPage("https://legal-hoster.example/impressum/acme"),
		}}
		imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if err != nil || !hasWarning(imp.Warnings, "different host (legal-hoster.example)") {
			t.Fatalf("imp=%v err=%v", imp, err)
		}
		if site.scrapes[1].Req.JSON != nil {
			t.Fatal("an external candidate must be checked before paying for the extraction")
		}
	})
}

func TestLookupRejectsNonImprintPages(t *testing.T) {
	tests := []struct {
		name string
		page *crawler.Page
	}{
		{"soft 404", &crawler.Page{URL: "https://acme.de/impressum", StatusCode: 200, Markdown: "# Willkommen\nUnsere Produkte"}},
		{"hard 404", &crawler.Page{URL: "https://acme.de/impressum", StatusCode: 404, Markdown: "Impressum 404"}},
		{"footer link only", &crawler.Page{URL: "https://acme.de/impressum", StatusCode: 200, Markdown: "# Seite nicht gefunden\n[Impressum](/impressum)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := &mockSite{pages: map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/"), "https://acme.de/impressum": tt.page}}
			if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v", err)
			}
			for _, c := range site.scrapes {
				if c.Req.JSON != nil {
					t.Fatal("no extraction may be paid for a rejected page")
				}
			}
		})
	}
	t.Run("marker words", func(t *testing.T) {
		english := "[Imprint](/pages/imprint)\n\ngitti GmbH, Kastanienallee 43a, 10435 Berlin\nRepresented by Founder & Managing Director: J. Doe"
		if reason := unusableReason(Extraction{StatusCode: 200, Text: english}); reason != "" {
			t.Fatalf("english imprint rejected: %s", reason)
		}
		if reason := unusableReason(Extraction{StatusCode: 200, Text: imprintMD}); reason != "" {
			t.Fatalf("imprint rejected: %s", reason)
		}
	})
}

func TestLookupErrors(t *testing.T) {
	t.Run("homepage failure", func(t *testing.T) {
		site := &mockSite{errs: map[string]error{"https://acme.de/": crawler.ErrUpstream, "https://www.acme.de/": crawler.ErrUpstream}}
		if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, crawler.ErrUpstream) {
			t.Fatalf("err = %v", err)
		}
	})
	for _, status := range []int{403, 404, 500, 503} {
		t.Run(fmt.Sprintf("homepage answers %d", status), func(t *testing.T) {
			site := &mockSite{pages: map[string]*crawler.Page{"https://acme.de/": {URL: "https://acme.de/", StatusCode: status}}}
			_, err := newSvc(site).Lookup(context.Background(), "acme.de")
			if !errors.Is(err, crawler.ErrUpstream) || errors.Is(err, ErrNotFound) || len(site.scrapes) != 2 { // apex, then www
				t.Fatalf("err=%v calls=%v", err, site.urls())
			}
		})
	}
	for name, cause := range map[string]error{
		"timeout": crawler.ErrUpstreamTimeout, "rate limit": crawler.ErrRateLimited,
		"account": crawler.ErrUpstreamUnavailable, "canceled": context.Canceled,
	} {
		t.Run("candidate "+name+" aborts the lookup", func(t *testing.T) {
			site := &mockSite{
				pages: map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/", "https://acme.de/impressum")},
				errs:  map[string]error{"https://acme.de/impressum": cause},
			}
			_, err := newSvc(site).Lookup(context.Background(), "acme.de")
			if !errors.Is(err, cause) || errors.Is(err, ErrNotFound) || len(site.scrapes) != 2 || site.maps != 0 {
				t.Fatalf("err=%v calls=%v maps=%d", err, site.urls(), site.maps)
			}
		})
		t.Run("site map "+name+" aborts the lookup", func(t *testing.T) {
			site := &mockSite{pages: map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/")}, mapErr: cause}
			if _, err := newSvc(site).Lookup(context.Background(), "acme.de"); !errors.Is(err, cause) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLookupResult(t *testing.T) {
	strong := func(page *crawler.Page) *mockSite {
		return &mockSite{pages: map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/", "https://acme.de/impressum"), "https://acme.de/impressum": page}}
	}
	t.Run("text is kept when the extraction returns nothing (partial)", func(t *testing.T) {
		page := imprintPage("https://acme.de/impressum")
		page.JSON = nil
		imp, err := newSvc(strong(page)).Lookup(context.Background(), "acme.de")
		if err != nil || imp.Text != imprintMD || imp.CompanyName != nil || !imp.PartialResult() || !hasWarning(imp.Warnings, WarnExtractionUnavailable) {
			t.Fatalf("imp=%+v err=%v", imp, err)
		}
	})
	t.Run("a failed extraction keeps the accepted page (partial)", func(t *testing.T) {
		site := &mockSite{
			pages:   map[string]*crawler.Page{"https://acme.de/": homepage("https://acme.de/", "https://acme.de/info"), "https://acme.de/info": imprintPage("https://acme.de/info")},
			jsonErr: fmt.Errorf("%w: HTTP 200 [SCRAPE_ALL_ENGINES_FAILED]", crawler.ErrUpstream),
		}
		site.pages["https://acme.de/"].Markdown = "[Impressum](https://acme.de/info)"
		imp, err := newSvc(site).Lookup(context.Background(), "acme.de")
		if err != nil || imp.Text != imprintMD || !imp.PartialResult() {
			t.Fatalf("imp=%+v err=%v", imp, err)
		}
	})
	t.Run("complete result is not partial", func(t *testing.T) {
		imp, err := newSvc(strong(imprintPage("https://acme.de/impressum"))).Lookup(context.Background(), "acme.de")
		if err != nil || imp.PartialResult() {
			t.Fatalf("imp=%+v err=%v", imp, err)
		}
	})
	t.Run("invented values are removed, the text stays", func(t *testing.T) {
		page := imprintPage("https://acme.de/impressum")
		page.JSON["vat_id"] = "DE999999999"
		imp, err := newSvc(strong(page)).Lookup(context.Background(), "acme.de")
		if err != nil || !hasWarning(imp.Warnings, "vat_id not found verbatim") || imp.VATID != nil || !strings.Contains(imp.Text, "DE123456789") {
			t.Fatalf("imp=%+v err=%v", imp, err)
		}
	})
	t.Run("huge text is truncated on a rune boundary", func(t *testing.T) {
		page := imprintPage("https://acme.de/impressum")
		page.Markdown = imprintMD + "\n" + strings.Repeat("ä", maxTextBytes)
		imp, err := newSvc(strong(page)).Lookup(context.Background(), "acme.de")
		if err != nil || len(imp.Text) > maxTextBytes || !utf8.ValidString(imp.Text) || !hasWarning(imp.Warnings, WarnTextTruncated) {
			t.Fatalf("err=%v len=%d", err, len(imp.Text))
		}
		if imp.VATID == nil {
			t.Error("validation must still see the full text")
		}
	})
	t.Run("canonical imprint URL", func(t *testing.T) {
		for _, tt := range [][3]string{
			{"https://www.dfg.de/impressum?enodia=eyJ.abc", "https://www.dfg.de/impressum", "https://www.dfg.de/impressum"},
			{"https://acme.de/impressum?lang=de", "https://acme.de/impressum?lang=de", "https://acme.de/impressum?lang=de"},
			{"https://acme.de/legal/impressum", "https://acme.de/impressum", "https://acme.de/legal/impressum"},
			{"", "https://acme.de/impressum", "https://acme.de/impressum"},
		} {
			if got := canonicalImprintURL(tt[0], tt[1]); got != tt[2] {
				t.Errorf("canonicalImprintURL(%q, %q) = %q, want %q", tt[0], tt[1], got, tt[2])
			}
		}
	})
}

func TestLookupParentDomain(t *testing.T) {
	t.Run("registrable parent", func(t *testing.T) {
		for host, want := range map[string]string{
			"eu.store.bambulab.com": "bambulab.com", "shop.example.de": "example.de", "a.b.c.example.co.uk": "example.co.uk",
			"example.de": "", "example.co.uk": "", "shop.myshopify.com": "", "user.blogspot.com": "",
		} {
			got, ok := parentDomain(host)
			if got != want || ok != (want != "") {
				t.Errorf("parentDomain(%q) = %q, %v; want %q", host, got, ok, want)
			}
		}
	})
	t.Run("imprint found on the parent via its homepage links", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{
			"https://eu.store.acme.de/":                     homepage("https://eu.store.acme.de/"),
			"https://acme.de/":                              homepage("https://acme.de/de-de", "https://acme.de/de-de/policies/page/impressum"),
			"https://acme.de/de-de/policies/page/impressum": imprintPage("https://acme.de/de-de/policies/page/impressum"),
		}}
		imp, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de")
		if err != nil || imp.ImprintURL != "https://acme.de/de-de/policies/page/impressum" {
			t.Fatalf("imp=%v err=%v calls=%v", imp, err, site.urls())
		}
		if !hasWarning(imp.Warnings, WarnParentDomain) || hasWarning(imp.Warnings, WarnExternalHost) {
			t.Fatalf("warnings = %+v", imp.Warnings)
		}
		if site.maps != 1 { // only the host itself gets the site map
			t.Fatalf("map calls = %d", site.maps)
		}
	})
	t.Run("parent without imprint keeps not found", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{"https://eu.store.acme.de/": homepage("https://eu.store.acme.de/"), "https://acme.de/": homepage("https://acme.de/")}}
		if _, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("parent site down keeps not found", func(t *testing.T) {
		site := &mockSite{pages: map[string]*crawler.Page{"https://eu.store.acme.de/": homepage("https://eu.store.acme.de/")},
			errs: map[string]error{"https://acme.de/": crawler.ErrUpstream, "https://www.acme.de/": crawler.ErrUpstream}}
		if _, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	for name, cause := range map[string]error{"timeout": crawler.ErrUpstreamTimeout, "rate limit": crawler.ErrRateLimited, "canceled": context.Canceled} {
		t.Run("parent "+name+" aborts", func(t *testing.T) {
			site := &mockSite{pages: map[string]*crawler.Page{"https://eu.store.acme.de/": homepage("https://eu.store.acme.de/")}, errs: map[string]error{"https://acme.de/": cause}}
			if _, err := newSvc(site).Lookup(context.Background(), "eu.store.acme.de"); !errors.Is(err, cause) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("hosted subdomains never fall back to the hoster", func(t *testing.T) {
		for _, host := range []string{"peters-shop.myshopify.com", "acme.blogspot.com"} {
			site := &mockSite{pages: map[string]*crawler.Page{"https://" + host + "/": homepage("https://" + host + "/")}}
			if _, err := newSvc(site).Lookup(context.Background(), host); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: err = %v", host, err)
			}
			for _, u := range site.urls() {
				if !strings.Contains(u, host) {
					t.Fatalf("%s: fetched %s", host, u)
				}
			}
		}
	})
}
