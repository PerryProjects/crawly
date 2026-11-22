// Package brand extracts brand data (logo, colors, fonts, description, tone of
// voice) of a website and maps Firecrawl's untyped "branding" payload into a
// stable DTO. No Firecrawl type leaks through the HTTP API.
//
// The branding payload shape is not typed by the SDK and was NOT verified
// against the live API; see docs/VERIFIED_APIS.md. The mapper therefore
// tolerates missing and differently typed fields.
package brand

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/crawler"
	"github.com/OWNER/siteinfo/internal/observability"
)

// Colors are the main brand colors as lowercase hex strings (#rrggbb or #rrggbbaa).
type Colors struct {
	Primary       *string `json:"primary"`
	Secondary     *string `json:"secondary"`
	Accent        *string `json:"accent"`
	Background    *string `json:"background"`
	TextPrimary   *string `json:"text_primary"`
	TextSecondary *string `json:"text_secondary"`
}

// Font is a font family with the role it plays on the site (primary, heading, code or "").
type Font struct {
	Family string `json:"family"`
	Role   string `json:"role,omitempty"`
}

// Typography summarizes the type scale. Sizes are passed through as written by Firecrawl.
type Typography struct {
	FontFamilies map[string]string `json:"font_families"`
	FontSizes    map[string]string `json:"font_sizes"`
	FontWeights  map[string]int    `json:"font_weights"`
}

// Brand is the cached and served brand result for one host.
type Brand struct {
	BrandName   *string    `json:"brand_name"`
	Logo        *string    `json:"logo"`
	Favicon     *string    `json:"favicon"`
	OGImage     *string    `json:"og_image"`
	ColorScheme *string    `json:"color_scheme"` // "light" or "dark"
	Colors      Colors     `json:"colors"`
	Fonts       []Font     `json:"fonts"`
	Typography  Typography `json:"typography"`
	Description *string    `json:"description"`
	// ToneOfVoice is a short keyword list. ToneSource says where it came from:
	// "extraction" (LLM reading the page text) or "firecrawl_hint" (Firecrawl's
	// personality guess, a hint rather than a fact).
	ToneOfVoice []string `json:"tone_of_voice"`
	ToneSource  *string  `json:"tone_source"`
	Language    *string  `json:"language"` // primary language subtag, e.g. "de"
	// Warnings lists what is missing because an upstream part failed. Never nil in output.
	Warnings []Warning `json:"warnings"`
}

// Warning has the same shape as imprint.Warning: a stable Code and a human Message.
type Warning struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Warning codes.
const (
	WarnBrandingUnavailable   = "branding_unavailable"   // Firecrawl returned no branding payload (logo, colors, fonts)
	WarnExtractionUnavailable = "extraction_unavailable" // the LLM extraction (description, tone, language) returned nothing
)

// PartialResult reports a degraded result, which the cache keeps only for the short partial TTL.
func (b *Brand) PartialResult() bool { return len(b.Warnings) > 0 }

// Scraper is what this package needs from the crawler.
type Scraper interface {
	Scrape(ctx context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error)
}

// Service fetches brand data for a host.
type Service struct {
	scraper Scraper
	timeout time.Duration // per-call timeout of the branding scrape, 0 = crawler default
	log     *zap.SugaredLogger
}

// Option configures optional Service settings.
type Option func(*Service)

// WithTimeout sets the per-call timeout of the branding scrape. Rendering the page for
// branding takes far longer than a plain scrape on large sites.
func WithTimeout(d time.Duration) Option { return func(s *Service) { s.timeout = d } }

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) Option { return func(s *Service) { s.log = l.Sugar() } }

// NewService returns a Service backed by scraper.
func NewService(scraper Scraper, opts ...Option) *Service {
	s := &Service{scraper: scraper, log: zap.NewNop().Sugar()}
	for _, o := range opts {
		o(s)
	}
	return s
}

const extractionPrompt = `From this homepage return:
- description: a neutral one or two sentence description of what the organization does. Write it in the main language of the page (German for a German page), never translate it. Use only what the page says.
- tone_of_voice: up to 5 short lowercase English keywords describing the writing style (e.g. professional, friendly, technical, playful, formal).
- language: the ISO 639-1 code of the main page language.
Return null for anything you cannot determine.`

func extractionSchema() map[string]any {
	str := map[string]any{"type": []string{"string", "null"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description":   str,
			"tone_of_voice": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"}},
			"language":      str,
		},
		"required":             []string{"description", "tone_of_voice", "language"},
		"additionalProperties": false,
	}
}

// Lookup scrapes the homepage of host (normalized, no scheme) and maps the result.
func (s *Service) Lookup(ctx context.Context, host string) (*Brand, error) {
	homepage := "https://" + host + "/"
	page, err := crawler.ScrapeHomepage(ctx, s.scraper, host, crawler.ScrapeRequest{
		Branding: true,
		Timeout:  s.timeout,
		JSON:     &crawler.JSONExtract{Prompt: extractionPrompt, Schema: extractionSchema()},
	})
	if err != nil {
		return nil, fmt.Errorf("brand: scrape homepage: %w", err)
	}
	base, err := url.Parse(page.URL)
	if err != nil || base.Host == "" {
		base, _ = url.Parse(homepage)
	}
	b := Map(page, base)
	keys := make([]string, 0, len(page.Branding))
	for k := range page.Branding {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	observability.TraceFrom(ctx).Add("brand", "homepage mapped", "final_url", page.URL, "status_code", page.StatusCode,
		"branding_keys", keys, "llm_extraction", page.JSON != nil, "logo_found", b.Logo != nil, "colors_found", b.Colors.Primary != nil,
		"tone_source", deref(b.ToneSource))
	if page.Branding == nil {
		s.log.Warnw("firecrawl returned no branding payload", observability.Fields(ctx), "host", host)
		b.Warnings = append(b.Warnings, Warning{Code: WarnBrandingUnavailable, Message: "Firecrawl returned no branding data; logo, colors and fonts are missing"})
	}
	if page.JSON == nil {
		b.Warnings = append(b.Warnings, Warning{Code: WarnExtractionUnavailable, Message: "the LLM extraction returned nothing; description, tone and language may be missing or come from page metadata"})
	}
	return b, nil
}

// Map converts a scraped page into the Brand DTO. base resolves relative URLs.
func Map(page *crawler.Page, base *url.URL) *Brand {
	br := page.Branding
	b := &Brand{
		Warnings:    []Warning{},
		Fonts:       []Font{},
		ToneOfVoice: []string{},
		Typography:  Typography{FontFamilies: map[string]string{}, FontSizes: map[string]string{}, FontWeights: map[string]int{}},
	}

	b.BrandName = clip(str(br, "brandName"), 200)
	images := object(br, "images")
	b.Logo = assetURL(base, first(str(images, "logo"), str(br, "logo")))
	b.Favicon = assetURL(base, str(images, "favicon"))
	b.OGImage = assetURL(base, first(str(images, "ogImage"), str(page.Metadata, "ogImage")))

	if cs := strings.ToLower(deref(str(br, "colorScheme"))); cs == "light" || cs == "dark" {
		b.ColorScheme = &cs
	}

	colors := object(br, "colors")
	b.Colors = Colors{
		Primary:       hexColor(str(colors, "primary")),
		Secondary:     hexColor(str(colors, "secondary")),
		Accent:        hexColor(str(colors, "accent")),
		Background:    hexColor(str(colors, "background")),
		TextPrimary:   hexColor(str(colors, "textPrimary")),
		TextSecondary: hexColor(str(colors, "textSecondary")),
	}

	mapTypography(b, object(br, "typography"), br["fonts"])

	extracted := page.JSON
	b.Description = first(clip(str(extracted, "description"), 500), clip(str(page.Metadata, "description"), 500), clip(str(page.Metadata, "ogDescription"), 500))
	b.Language = first(language(str(extracted, "language")), language(str(page.Metadata, "language")))

	if tones := keywords(extracted["tone_of_voice"]); len(tones) > 0 {
		b.ToneOfVoice, b.ToneSource = tones, ptr("extraction")
	} else if tones := keywords(object(br, "personality")["tone"]); len(tones) > 0 {
		b.ToneOfVoice, b.ToneSource = tones, ptr("firecrawl_hint")
	}
	return b
}
