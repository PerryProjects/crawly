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
	Warnings []string `json:"warnings"`
}

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
