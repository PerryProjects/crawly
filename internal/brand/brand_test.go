package brand

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/OWNER/siteinfo/internal/crawler"
)

// fixture follows the branding shape shown in the Firecrawl docs cookbook.
// It is illustrative, NOT a captured live response.
const brandingFixture = `{
  "colorScheme": "Light",
  "logo": "https://acme.de/legacy-logo.png",
  "images": {"logo": "/assets/logo.svg", "favicon": "//acme.de/favicon.ico", "ogImage": "https://acme.de/og.jpg"},
  "colors": {"primary": "#0A66C2", "secondary": "#FFF", "accent": "rgb(1,2,3)", "background": "#ffffff", "textPrimary": "#111111", "textSecondary": "red"},
  "typography": {
    "fontFamilies": {"primary": "Inter", "heading": "Playfair Display", "code": "Fira Code"},
    "fontSizes": {"h1": "48px", "body": 16},
    "fontWeights": {"regular": 400, "bold": "700"}
  },
  "fonts": [{"family": "Inter"}, "Roboto", {"family": ""}],
  "personality": {"tone": "Professional, Corporate", "energy": "low"}
}`

func fixturePage(t *testing.T, extra map[string]any) *crawler.Page {
	t.Helper()
	var br map[string]any
	if err := json.Unmarshal([]byte(brandingFixture), &br); err != nil {
		t.Fatal(err)
	}
	return &crawler.Page{URL: "https://www.acme.de/", Branding: br, JSON: extra, Metadata: map[string]any{"description": "Meta description", "language": "de-DE"}}
}

func baseURL() *url.URL { u, _ := url.Parse("https://www.acme.de/"); return u }

func eq(t *testing.T, name string, got *string, want string) {
	t.Helper()
	switch {
	case want == "" && got != nil:
		t.Errorf("%s = %q, want nil", name, *got)
	case want != "" && (got == nil || *got != want):
		t.Errorf("%s = %v, want %q", name, got, want)
	}
}

func TestMapFullFixture(t *testing.T) {
	b := Map(fixturePage(t, map[string]any{
		"description":   "ACME baut Raketen.",
		"tone_of_voice": []any{"Professional", "friendly", "professional", "  ", "a very long keyword that exceeds thirty characters"},
		"language":      "de",
	}), baseURL())

	eq(t, "logo", b.Logo, "https://www.acme.de/assets/logo.svg") // images.logo wins, relative URL resolved
	eq(t, "favicon", b.Favicon, "https://acme.de/favicon.ico")   // protocol-relative
	eq(t, "og", b.OGImage, "https://acme.de/og.jpg")
	eq(t, "scheme", b.ColorScheme, "light")
	eq(t, "primary", b.Colors.Primary, "#0a66c2")
	eq(t, "secondary", b.Colors.Secondary, "#ffffff") // #fff expanded
	eq(t, "accent", b.Colors.Accent, "")              // rgb() dropped, not guessed
	eq(t, "text secondary", b.Colors.TextSecondary, "")
	eq(t, "description", b.Description, "ACME baut Raketen.")
	eq(t, "language", b.Language, "de")
	eq(t, "tone source", b.ToneSource, "extraction")
	if !slices.Equal(b.ToneOfVoice, []string{"professional", "friendly"}) {
		t.Errorf("tone = %v", b.ToneOfVoice)
	}
	wantFonts := []Font{{"Inter", "primary"}, {"Playfair Display", "heading"}, {"Fira Code", "code"}, {"Inter", ""}, {"Roboto", ""}}
	if !slices.Equal(b.Fonts, wantFonts) {
		t.Errorf("fonts = %v", b.Fonts)
	}
	if b.Typography.FontSizes["h1"] != "48px" || b.Typography.FontSizes["body"] != "16" || b.Typography.FontWeights["bold"] != 700 || b.Typography.FontFamilies["heading"] != "Playfair Display" {
		t.Errorf("typography = %+v", b.Typography)
	}
}

func TestMapFallbacks(t *testing.T) {
	// No LLM extraction: description and language fall back to page metadata, tone to Firecrawl's hint.
	b := Map(fixturePage(t, nil), baseURL())
	eq(t, "description", b.Description, "Meta description")
	eq(t, "language", b.Language, "de")
	eq(t, "tone source", b.ToneSource, "firecrawl_hint")
	if !slices.Equal(b.ToneOfVoice, []string{"professional", "corporate"}) {
		t.Errorf("tone = %v", b.ToneOfVoice)
	}
}

func TestMapEmptyAndHostile(t *testing.T) {
	b := Map(&crawler.Page{}, baseURL())
	if b.Logo != nil || b.Description != nil || b.ToneSource != nil || b.Fonts == nil || b.ToneOfVoice == nil || b.Typography.FontSizes == nil {
		t.Fatalf("empty page must yield nulls and empty (non-nil) collections: %+v", b)
	}
	hostile := &crawler.Page{Branding: map[string]any{
		"images":      map[string]any{"logo": "data:image/png;base64,AAAA", "favicon": "javascript:alert(1)", "ogImage": "ftp://x/y.png"},
		"colors":      "not an object",
		"typography":  []any{1, 2},
		"fonts":       "Arial",
		"colorScheme": "purple",
	}}
	b = Map(hostile, baseURL())
	if b.Logo != nil || b.Favicon != nil || b.OGImage != nil || b.ColorScheme != nil || len(b.Fonts) != 0 {
		t.Fatalf("hostile input leaked: %+v", b)
	}
}

func TestLanguageAndHelpers(t *testing.T) {
	for in, want := range map[string]string{"de": "de", "DE-at": "de", "en_US": "en", "deu": "deu", "german": "", "": ""} {
		var p *string
		if in != "" {
			p = &in
		}
		eq(t, "language("+in+")", language(p), want)
	}
	if got := clip(ptr("äöü"), 2); *got != "äö" {
		t.Errorf("clip = %q", *got)
	}
}

type fakeScraper struct {
	page *crawler.Page
	err  error
	req  crawler.ScrapeRequest
	url  string
}

func (f *fakeScraper) Scrape(_ context.Context, u string, req crawler.ScrapeRequest) (*crawler.Page, error) {
	f.url, f.req = u, req
	return f.page, f.err
}

func TestServiceLookup(t *testing.T) {
	fs := &fakeScraper{page: fixturePage(t, map[string]any{"description": "Hallo"})}
	b, err := NewService(fs).Lookup(context.Background(), "acme.de")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "description", b.Description, "Hallo")
	if fs.url != "https://acme.de/" || !fs.req.Branding || fs.req.JSON == nil || fs.req.JSON.Schema == nil {
		t.Fatalf("scrape request = %s %+v", fs.url, fs.req)
	}

	fs = &fakeScraper{err: crawler.ErrUpstreamTimeout}
	if _, err := NewService(fs).Lookup(context.Background(), "acme.de"); !errors.Is(err, crawler.ErrUpstreamTimeout) {
		t.Fatalf("err = %v", err)
	}
}
