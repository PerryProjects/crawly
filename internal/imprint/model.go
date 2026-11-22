// Package imprint finds and extracts the legal notice ("Impressum") of a
// DACH website: page discovery, extraction, deterministic validation.
//
// The extracted fields come from an LLM and may be wrong. Imprint.Text, the raw
// page markdown, is the legally authoritative part of every result.
package imprint

import (
	"errors"

	"go.uber.org/zap/zapcore"
)

// ErrNotFound means no imprint page could be located. It is a normal,
// negative-cacheable outcome, not a failure.
var ErrNotFound = errors.New("imprint: no imprint page found")

// Address is a postal address. All fields are nullable.
type Address struct {
	Street     *string `json:"street"`
	PostalCode *string `json:"postal_code"`
	City       *string `json:"city"`
	Country    *string `json:"country"`
}

// Contact holds contact channels. All fields are nullable.
type Contact struct {
	Email *string `json:"email"`
	Phone *string `json:"phone"`
	Fax   *string `json:"fax"`
}

// Register is a commercial register entry. All fields are nullable.
type Register struct {
	Type   *string `json:"type"`
	Court  *string `json:"court"`
	Number *string `json:"number"`
}

// Fields are the structured values extracted from the imprint page.
// Absent information is nil, never a guess.
type Fields struct {
	CompanyName           *string   `json:"company_name"`
	LegalForm             *string   `json:"legal_form"`
	RepresentedBy         []string  `json:"represented_by"`
	Address               *Address  `json:"address"`
	Contact               *Contact  `json:"contact"`
	Register              *Register `json:"register"`
	VATID                 *string   `json:"vat_id"`
	SupervisoryAuthority  *string   `json:"supervisory_authority"`
	ResponsibleForContent *string   `json:"responsible_for_content"`
	Country               *string   `json:"country"` // DE, AT or CH
}

// Imprint is the cached and served result for one host.
type Imprint struct {
	ImprintURL string `json:"imprint_url"`
	Fields
	// Text is the raw page markdown. It is authoritative; Fields are a convenience.
	Text string `json:"text"`
	// Warnings lists failed deterministic checks and caveats. Never nil in output.
	Warnings []Warning `json:"warnings"`
}

// Warning is a machine readable finding about the result. Code and Field are stable and meant
// for callers (e.g. to highlight a field for review); Message is for humans and may change.
type Warning struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"` // dotted JSON path, e.g. "contact.email"
	Message string `json:"message"`
}

// Warning codes.
const (
	WarnMissing               = "missing"                // an expected field was not found on the page
	WarnInvalidFormat         = "invalid_format"         // value kept, but it does not match the format for its country
	WarnInvalidRemoved        = "invalid_removed"        // value was unusable and has been set to null
	WarnUnverified            = "unverified"             // value kept, but not found verbatim in the page text
	WarnUnverifiedRemoved     = "unverified_removed"     // identifier not found verbatim in the page text, set to null
	WarnCountryMismatch       = "country_mismatch"       // value belongs to another country than the imprint
	WarnNotDACH               = "not_dach"               // country outside DE/AT/CH, set to null
	WarnExtractionUnavailable = "extraction_unavailable" // the LLM extraction returned nothing; only the text is usable
	WarnParentDomain          = "parent_domain"          // imprint found on the parent domain, not on the host itself
	WarnExternalHost          = "external_host"          // imprint page is hosted on an unrelated host
	WarnNormalized            = "normalized"             // value was rewritten deterministically (e.g. "a [at] b.de" -> "a@b.de")
	WarnTextTruncated         = "text_truncated"         // the page text exceeded the size limit and was cut
)

// MarshalLogObject implements zapcore.ObjectMarshaler. It deliberately leaves out every
// personal or identifying field (names, address, e-mail, phone, IDs) and the page text:
// imprints of sole proprietors are personal data and must not end up in logs.
func (i *Imprint) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("imprint_url", i.ImprintURL)
	if i.Country != nil {
		enc.AddString("country", *i.Country)
	}
	enc.AddInt("warnings", len(i.Warnings))
	enc.AddInt("text_len", len(i.Text))
	return nil
}

// PartialResult reports a degraded result (the LLM extraction failed; only the text is usable).
// The cache keeps it for the short partial TTL so a transient failure is retried soon.
func (i *Imprint) PartialResult() bool {
	for _, w := range i.Warnings {
		if w.Code == WarnExtractionUnavailable {
			return true
		}
	}
	return false
}
