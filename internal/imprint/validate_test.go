package imprint

import (
	"slices"
	"strings"
	"testing"
)

func s(v string) *string { return &v }

func hasWarning(ws []string, sub string) bool {
	return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(w, sub) })
}

func TestValidateVAT(t *testing.T) {
	tests := []struct {
		name, vat, country string
		wantVAT            string // normalized output
		warn               string // substring of expected warning, "" for none
	}{
		{"DE ok", "DE123456789", "DE", "DE123456789", ""},
		{"DE with spaces is normalized", "DE 123 456 789", "DE", "DE123456789", ""},
		{"DE lower case", "de123456789", "DE", "DE123456789", ""},
		{"DE too short", "DE12345678", "DE", "DE12345678", "vat_id"},
		{"DE letters", "DE12345678X", "DE", "DE12345678X", "vat_id"},
		{"AT ok", "ATU12345678", "AT", "ATU12345678", ""},
		{"AT without U", "AT12345678", "AT", "AT12345678", "vat_id"},
		{"AT too long", "ATU123456789", "AT", "ATU123456789", "vat_id"},
		{"CH ok", "CHE-123.456.789", "CH", "CHE-123.456.789", ""},
		{"CH with MWST", "CHE-123.456.789 MWST", "CH", "CHE-123.456.789 MWST", ""},
		{"CH with TVA no space", "CHE-123.456.789TVA", "CH", "CHE-123.456.789 TVA", ""},
		{"CH without separators", "CHE123456789", "CH", "CHE123456789", "vat_id"},
		{"country mismatch", "ATU12345678", "DE", "ATU12345678", "format but the imprint country"},
		{"unknown prefix", "FR12345678901", "", "FR12345678901", "vat_id"},
		{"country inferred from prefix, no mismatch", "DE123456789", "", "DE123456789", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Fields{CompanyName: s("ACME"), Address: &Address{}, VATID: s(tt.vat)}
			if tt.country != "" {
				f.Country = s(tt.country)
			}
			ws := Validate(f, "") // empty text disables the grounding check
			if *f.VATID != tt.wantVAT {
				t.Errorf("vat = %q, want %q", *f.VATID, tt.wantVAT)
			}
			if tt.warn == "" && len(ws) != 0 {
				t.Errorf("unexpected warnings: %v", ws)
			}
			if tt.warn != "" && !hasWarning(ws, tt.warn) {
				t.Errorf("warnings %v lack %q", ws, tt.warn)
			}
		})
	}
}

func TestValidateRegister(t *testing.T) {
	tests := []struct {
		name    string
		country string
		typ     string
		number  string
		wantTyp string
		warn    string
	}{
		{"HRB ok", "DE", "HRB", "12345", "HRB", ""},
		{"HRB with court letter", "DE", "HRB", "12345 B", "HRB", ""},
		{"lower case type", "DE", "hrb", "12345", "HRB", ""},
		{"GnR spelling", "DE", "GNR", "123", "GnR", ""},
		{"HRA ok", "DE", "HRA", "9876", "HRA", ""},
		{"VR ok", "DE", "VR", "1234", "VR", ""},
		{"PR ok", "DE", "PR", "77", "PR", ""},
		{"DE number garbage", "DE", "HRB", "abc", "HRB", "register.number"},
		{"DE unknown type", "DE", "XYZ", "12345", "XYZ", "register.type"},
		{"FN ok", "AT", "FN", "123456a", "FN", ""},
		{"FN with space", "AT", "FN", "123456 a", "FN", ""},
		{"FN without letter", "AT", "FN", "123456", "FN", "Firmenbuch"},
		{"FN in DE", "DE", "FN", "123456a", "FN", "belongs to AT"},
		{"HRB in AT", "AT", "HRB", "12345", "HRB", "belongs to DE"},
		{"CH is not pattern checked", "CH", "Handelsregister", "CH-020.3.123.456-7", "Handelsregister", ""},
		{"number without type", "DE", "", "12345", "", "without register.type"},
		{"type glued to number", "DE", "HRB", "hrb 104144", "HRB", ""},
		{"glued number, no type given", "DE", "", "HRB 104144", "HRB", ""},
		{"glued FN", "AT", "", "FN 123456a", "FN", ""},
		{"glued number of another type", "DE", "HRA", "HRB 1234", "HRA", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Register{Number: s(tt.number)}
			if tt.typ != "" {
				r.Type = s(tt.typ)
			}
			f := &Fields{CompanyName: s("ACME"), Address: &Address{}, Country: s(tt.country), Register: r}
			ws := Validate(f, "")
			if tt.wantTyp != "" && (f.Register.Type == nil || *f.Register.Type != tt.wantTyp) {
				t.Errorf("type = %v, want %s", f.Register.Type, tt.wantTyp)
			}
			if tt.warn == "" && len(ws) != 0 {
				t.Errorf("unexpected warnings: %v", ws)
			}
			if tt.warn != "" && !hasWarning(ws, tt.warn) {
				t.Errorf("warnings %v lack %q", ws, tt.warn)
			}
		})
	}
}

func TestValidateEmailPostalCountry(t *testing.T) {
	base := func() *Fields {
		return &Fields{CompanyName: s("ACME"), Address: &Address{PostalCode: s("10115")}, Contact: &Contact{Email: s("info@acme.de")}, Country: s("Deutschland")}
	}
	t.Run("clean", func(t *testing.T) {
		f := base()
		if ws := Validate(f, ""); len(ws) != 0 || *f.Country != "DE" {
			t.Fatalf("ws=%v country=%v", ws, *f.Country)
		}
	})
	for name, email := range map[string]string{
		"no domain dot":  "info@acme",
		"display name":   "Max <info@acme.de>",
		"two addresses":  "a@acme.de, b@acme.de",
		"no at":          "info.acme.de",
		"trailing dot":   "info@acme.de.",
		"spaces inside":  "in fo@acme.de",
		"text not email": "siehe Kontaktformular",
	} {
		t.Run("bad email "+name, func(t *testing.T) {
			f := base()
			f.Contact.Email = s(email)
			ws := Validate(f, "")
			if !hasWarning(ws, "contact.email") {
				t.Fatalf("no email warning for %q: %v", email, ws)
			}
			if f.Contact != nil && f.Contact.Email != nil {
				t.Fatalf("invalid email %q must be removed", email)
			}
		})
	}
	t.Run("postal code per country", func(t *testing.T) {
		for _, tt := range []struct {
			country, pc string
			bad         bool
		}{
			{"DE", "10115", false}, {"DE", "1011", true}, {"AT", "1010", false}, {"AT", "10115", true},
			{"CH", "8001", false}, {"CH", "80010", true},
		} {
			f := base()
			f.Country, f.Address.PostalCode = s(tt.country), s(tt.pc)
			if got := hasWarning(Validate(f, ""), "postal_code"); got != tt.bad {
				t.Errorf("%s %s: warning = %v, want %v", tt.country, tt.pc, got, tt.bad)
			}
		}
	})
	t.Run("non DACH country is dropped with warning", func(t *testing.T) {
		f := base()
		f.Country = s("France")
		ws := Validate(f, "")
		if f.Country != nil || !hasWarning(ws, "DACH") {
			t.Fatalf("country=%v ws=%v", f.Country, ws)
		}
	})
	t.Run("missing company and address", func(t *testing.T) {
		ws := Validate(&Fields{}, "")
		if !hasWarning(ws, "company_name not found") || !hasWarning(ws, "postal address not found") {
			t.Fatalf("ws=%v", ws)
		}
	})
}

func TestGroundingCheck(t *testing.T) {
	text := "# Impressum\n\nACME Beispiel GmbH\nMusterstraße 1, 10115 Berlin\nTel. +49 (30) 123-456\nE-Mail: info@acme-beispiel.de\nUSt-IdNr.: DE 123 456 789\nHRB 12345 B"
	f := &Fields{
		CompanyName: s("ACME Beispiel GmbH"),
		Address:     &Address{PostalCode: s("10115")},
		Contact:     &Contact{Email: s("info@acme-beispiel.de")},
		VATID:       s("DE123456789"),
		Register:    &Register{Type: s("HRB"), Number: s("12345 B")},
		Country:     s("DE"),
	}
	if ws := Validate(f, text); len(ws) != 0 {
		t.Fatalf("grounded values flagged: %v", ws)
	}

	f = &Fields{
		CompanyName: s("Phantasie AG"),
		Address:     &Address{PostalCode: s("80331")},
		Contact:     &Contact{Email: s("hallo@phantasie.de")},
		VATID:       s("DE999999999"),
		Register:    &Register{Type: s("HRB"), Number: s("99999")},
		Country:     s("DE"),
	}
	f.Contact.Phone = s("0800 000000")
	ws := Validate(f, text)
	for _, want := range []string{"company_name not found verbatim", "contact.email not found verbatim", "vat_id not found verbatim", "register.number not found verbatim", "contact.phone not found verbatim", "postal_code not found"} {
		if !hasWarning(ws, want) {
			t.Errorf("missing warning %q in %v", want, ws)
		}
	}
	// Invented identifiers are removed, not just flagged; descriptive fields are only flagged.
	if f.VATID != nil || f.Contact != nil || f.Register.Number != nil {
		t.Errorf("ungrounded identifiers must be removed: vat=%v contact=%v register=%+v", f.VATID, f.Contact, f.Register)
	}
	if f.CompanyName == nil || f.Address == nil || f.Register.Type == nil {
		t.Errorf("company name, address and register type must stay: %+v", f)
	}
}
