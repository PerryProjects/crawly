package imprint

import (
	"context"
	"fmt"

	"github.com/OWNER/siteinfo/internal/crawler"
)

// Scraper is what this package needs from the crawler.
type Scraper interface {
	Scrape(ctx context.Context, url string, req crawler.ScrapeRequest) (*crawler.Page, error)
}

// Extraction is the raw outcome of reading one imprint page.
type Extraction struct {
	URL        string // final URL after redirects
	StatusCode int    // target site status, 0 if unknown
	Text       string // raw markdown
	Fields     Fields
	Warnings   []string // problems with the structured part (page text is still usable)
}

// Extractor turns one imprint page into text plus structured fields.
// It is an interface so a registry based verification step (Handelsregister,
// Firmenbuch, Zefix) can wrap or replace the Firecrawl implementation later.
type Extractor interface {
	Extract(ctx context.Context, pageURL string) (Extraction, error)
}

// FirecrawlExtractor reads a candidate page in two steps. Step 1 fetches only the markdown,
// which is cheap and fast, and checks that the page really is an imprint (success status,
// imprint marker words). Only then step 2 asks Firecrawl for the LLM based JSON extraction.
// Probing a wrong candidate (a 404, a soft 404) therefore costs one fast call instead of a slow,
// credit-consuming extraction; measured live, extraction on a 404 page took about 8 s.
type FirecrawlExtractor struct{ scraper Scraper }

// NewFirecrawlExtractor returns an Extractor backed by scraper.
func NewFirecrawlExtractor(scraper Scraper) *FirecrawlExtractor {
	return &FirecrawlExtractor{scraper: scraper}
}

// Extract implements Extractor.
func (e *FirecrawlExtractor) Extract(ctx context.Context, pageURL string) (Extraction, error) {
	// FullPage: the legal text is the point; do not let a main-content heuristic cut it.
	page, err := e.scraper.Scrape(ctx, pageURL, crawler.ScrapeRequest{Markdown: true, FullPage: true})
	if err != nil {
		return Extraction{}, fmt.Errorf("imprint: scrape %s: %w", pageURL, err)
	}
	x := Extraction{URL: page.URL, StatusCode: page.StatusCode, Text: page.Markdown}
	if unusableReason(x) != "" {
		return x, nil // the service rejects it with the same reason; no extraction is paid for
	}

	page, err = e.scraper.Scrape(ctx, pageURL, crawler.ScrapeRequest{
		FullPage: true,
		JSON:     &crawler.JSONExtract{Prompt: extractionPrompt, Schema: extractionSchema()},
	})
	if err != nil {
		return Extraction{}, fmt.Errorf("imprint: extract %s: %w", pageURL, err)
	}
	if page.JSON == nil {
		x.Warnings = append(x.Warnings, "structured extraction returned no data")
		return x, nil
	}
	x.Fields = FieldsFromMap(page.JSON)
	return x, nil
}

const extractionPrompt = `Extract the legal notice (Impressum / Anbieterkennung / Offenlegung) of the website operator from this page.
Rules:
- Use only information that is explicitly written on the page.
- If a value is not stated, return null. Never guess, infer, complete or correct values.
- Copy the VAT ID (USt-IdNr./UID), the register number, e-mail address and phone numbers exactly as written.
- represented_by lists the natural persons named as legal representatives (e.g. Geschäftsführer, Vorstand, Inhaber).
- legal_form is the legal form of the operator (e.g. GmbH, AG, UG, e.U., Einzelunternehmen) if stated.
- register.type is the register abbreviation as written (e.g. HRB, HRA, GnR, PR, VR, FN); register.court is the register court.
- country is the ISO code DE, AT or CH of the operator's address, only if clearly stated.
- responsible_for_content is the person named as responsible for content (V.i.S.d.P. / § 18 MStV / Medieninhaber).`

// extractionSchema returns the JSON schema sent to Firecrawl. All fields are nullable.
func extractionSchema() map[string]any {
	str := map[string]any{"type": []string{"string", "null"}}
	obj := func(keys ...string) map[string]any {
		props := map[string]any{}
		for _, k := range keys {
			props[k] = str
		}
		return map[string]any{"type": []string{"object", "null"}, "properties": props, "required": keys, "additionalProperties": false}
	}
	keys := []string{
		"company_name", "legal_form", "represented_by", "address", "contact", "register",
		"vat_id", "supervisory_authority", "responsible_for_content", "country",
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"company_name":            str,
			"legal_form":              str,
			"represented_by":          map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"}},
			"address":                 obj("street", "postal_code", "city", "country"),
			"contact":                 obj("email", "phone", "fax"),
			"register":                obj("type", "court", "number"),
			"vat_id":                  str,
			"supervisory_authority":   str,
			"responsible_for_content": str,
			"country":                 map[string]any{"type": []string{"string", "null"}, "enum": []any{"DE", "AT", "CH", nil}},
		},
		"required":             keys,
		"additionalProperties": false,
	}
}
