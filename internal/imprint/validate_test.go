package imprint

import (
	"slices"
	"strings"
	"testing"
)

func s(v string) *string { return &v }

func hasWarning(ws []Warning, sub string) bool {
	return slices.ContainsFunc(ws, func(w Warning) bool { return strings.Contains(w.Message, sub) || w.Code == sub })
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
			f := &Fields{CompanyName: s("ACME"), Address: &Address{City: s("Berlin")}, VATID: s(tt.vat)}
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
		{"spaced digit groups", "DE", "HRA", "6 11 12", "HRA", ""},
		{"generic register name", "DE", "Handelsregister", "166842", "Handelsregister", "does not state the type"},
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
			f := &Fields{CompanyName: s("ACME"), Address: &Address{City: s("Berlin")}, Country: s(tt.country), Register: r}
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
	for _, want := range []string{"company_name not found verbatim", "contact.email not found verbatim", "vat_id not found verbatim", "register.number not found verbatim", "contact.phone not found", "postal_code not found"} {
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

func TestWarningCodesAndFields(t *testing.T) {
	f := &Fields{
		CompanyName: s("ACME GmbH"), Address: &Address{PostalCode: s("123")}, Country: s("DE"),
		VATID: s("ATU12345678"), Contact: &Contact{Email: s("not-an-email")},
	}
	got := map[string]string{}
	for _, w := range Validate(f, "") {
		if w.Message == "" {
			t.Errorf("warning without message: %+v", w)
		}
		got[w.Field] = w.Code
	}
	want := map[string]string{
		"address.postal_code": WarnInvalidFormat,
		"vat_id":              WarnCountryMismatch,
		"contact.email":       WarnInvalidRemoved,
	}
	for field, code := range want {
		if got[field] != code {
			t.Errorf("field %s: code %q, want %q (all: %v)", field, got[field], code, got)
		}
	}
	missing := Validate(&Fields{}, "")
	if missing[0].Code != WarnMissing || missing[0].Field != "company_name" {
		t.Errorf("missing = %+v", missing)
	}
}

func TestEmailDeobfuscation(t *testing.T) {
	tests := []struct {
		page, extracted, want, code string
	}{
		{"E-Mail: webmaster [at] tagesschau.de", "webmaster [at] tagesschau.de", "webmaster@tagesschau.de", WarnNormalized},
		{"info (at) acme (dot) de", "info (at) acme (dot) de", "info@acme.de", WarnNormalized},
		{"kontakt{ät}acme.de", "kontakt{ät}acme.de", "kontakt@acme.de", WarnNormalized},
		{"info@acme.de", "info@acme.de", "info@acme.de", ""},
		{"info at acme.de", "info at acme.de", "", WarnInvalidRemoved},   // bare "at" is never rewritten
		{"nothing here", "info [at] acme.de", "", WarnUnverifiedRemoved}, // grounding uses the value as written
	}
	for _, tt := range tests {
		f := &Fields{CompanyName: s("ACME"), Address: &Address{City: s("Berlin")}, Contact: &Contact{Email: s(tt.extracted)}}
		ws := Validate(f, "ACME "+tt.page)
		var got string
		if f.Contact != nil && f.Contact.Email != nil {
			got = *f.Contact.Email
		}
		if got != tt.want {
			t.Errorf("%q: email = %q, want %q", tt.extracted, got, tt.want)
		}
		if tt.code == "" && len(ws) != 0 || tt.code != "" && !hasWarning(ws, tt.code) {
			t.Errorf("%q: warnings = %+v, want code %q", tt.extracted, ws, tt.code)
		}
	}
}

func TestLegalFormCasing(t *testing.T) {
	for in, want := range map[string]string{
		"gmbh": "GmbH", "GMBH & CO. KG": "GmbH & Co. KG", "ug (haftungsbeschränkt)": "UG (haftungsbeschränkt)",
		"e.k.": "e.K.", "GmbH": "GmbH", "Ltd.": "Ltd.", "Gesellschaft mit beschränkter Haftung": "Gesellschaft mit beschränkter Haftung",
	} {
		f := &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}, LegalForm: s(in)}
		Validate(f, "")
		if *f.LegalForm != want {
			t.Errorf("legal form %q -> %q, want %q", in, *f.LegalForm, want)
		}
	}
}

func TestRealWorldIdentifierSpellings(t *testing.T) {
	// got-bag.com: two VAT IDs in one line.
	f := &Fields{CompanyName: s("GOT BAG GmbH"), Address: &Address{City: s("Berlin")}, Country: s("DE"), VATID: s("DE 326114858, CHE-276.969.644")}
	ws := Validate(f, "GOT BAG GmbH USt-IdNr.: DE 326114858, CHE-276.969.644")
	if f.VATID == nil || *f.VATID != "DE326114858" || len(f.OtherVATIDs) != 1 || f.OtherVATIDs[0] != "CHE-276.969.644" || !hasWarning(ws, WarnNormalized) {
		t.Fatalf("multi VAT: vat=%v others=%v ws=%+v", f.VATID, f.OtherVATIDs, ws)
	}
	// armedangels.com: "HR B 59603".
	f = &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}, Country: s("DE"), Register: &Register{Type: s("HRB"), Number: s("HR B 59603")}}
	if ws := Validate(f, "x HR B 59603"); len(ws) != 0 || *f.Register.Number != "59603" || *f.Register.Type != "HRB" {
		t.Fatalf("spaced register: %+v %+v", f.Register, ws)
	}
	// giesswein.com: "Firmenbuchnummer: 42354k" without a type.
	f = &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}, Country: s("AT"), Register: &Register{Number: s("42354k")}}
	ws = Validate(f, "x Firmenbuchnummer: 42354k")
	if f.Register.Type == nil || *f.Register.Type != "FN" || hasWarning(ws, "without register.type") {
		t.Fatalf("firmenbuch: %+v %+v", f.Register, ws)
	}
	// Without the word Firmenbuch on the page nothing is inferred.
	f = &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}, Country: s("AT"), Register: &Register{Number: s("42354k")}}
	Validate(f, "x Registernummer 42354k")
	if f.Register.Type != nil {
		t.Fatal("type must not be guessed")
	}
}

func TestSwissUIDAsRegister(t *testing.T) {
	f := &Fields{CompanyName: s("BLACKROLL AG"), Address: &Address{PostalCode: s("8598")}, Country: s("CH"),
		Register: &Register{Court: s("Canton of Thurgau"), Number: s("CHE-228.574.355")}}
	ws := Validate(f, "BLACKROLL AG 8598 UID: CHE-228.574.355")
	if f.Register.Type == nil || *f.Register.Type != "UID" || hasWarning(ws, "without register.type") || hasWarning(ws, WarnInvalidFormat) {
		t.Fatalf("register=%+v ws=%+v", f.Register, ws)
	}
}

func TestRegisterTypeFromPageText(t *testing.T) {
	f := &Fields{CompanyName: s("Bergfreunde GmbH"), Address: &Address{City: s("Berlin")}, Country: s("DE"), Register: &Register{Type: s("HR"), Number: s("382746")}}
	ws := Validate(f, "Bergfreunde GmbH Handelsregisternummer: HR Amtsgericht Stuttgart Registernummer: HRB 382746")
	if *f.Register.Type != "HRB" || !hasWarning(ws, WarnNormalized) || hasWarning(ws, WarnInvalidFormat) {
		t.Fatalf("register=%+v ws=%+v", f.Register, ws)
	}
	f = &Fields{CompanyName: s("dm"), Address: &Address{City: s("Berlin")}, Country: s("DE"), Register: &Register{Type: s("HRA"), Number: s("HR A 102711")}}
	if ws := Validate(f, "dm Registergericht Mannheim HR A 102711"); len(ws) != 0 || *f.Register.Number != "102711" {
		t.Fatalf("dm: register=%+v ws=%+v", f.Register, ws)
	}
	// The number must stand right after the type: no guessing from other numbers on the page.
	f = &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}, Country: s("DE"), Register: &Register{Number: s("123")}}
	Validate(f, "x HRB 999 and later 123")
	if f.Register.Type != nil {
		t.Fatalf("type guessed: %+v", f.Register)
	}
}

func TestLiveSpellingsRound2(t *testing.T) {
	// porsche.com: "HRB-Nr. 730623".
	f := &Fields{CompanyName: s("Porsche AG"), Address: &Address{City: s("Berlin")}, Country: s("DE"), Register: &Register{Type: s("HRB"), Number: s("HRB-Nr. 730623")}}
	if ws := Validate(f, "Porsche AG Registergericht: Amtsgericht Stuttgart HRB-Nr. 730623"); hasWarning(ws, WarnInvalidFormat) || *f.Register.Number != "730623" {
		t.Fatalf("porsche: %+v %+v", f.Register, ws)
	}
	// blackroll.com: Cloudflare placeholder, one mailto link on the page.
	text := "BLACKROLL AG Email: [shop.de@blackroll.com](mailto:shop.de@blackroll.com) Website"
	f = &Fields{CompanyName: s("BLACKROLL AG"), Address: &Address{City: s("Berlin")}, Contact: &Contact{Email: s("[email protected]")}}
	Validate(f, text+" [email protected]")
	if f.Contact == nil || f.Contact.Email == nil || *f.Contact.Email != "shop.de@blackroll.com" {
		t.Fatalf("blackroll: %+v", f.Contact)
	}
	// Two different mailto addresses: nothing is picked.
	f = &Fields{CompanyName: s("x"), Address: &Address{City: s("Berlin")}}
	Validate(f, "x [a](mailto:a@x.de) [b](mailto:b@x.de)")
	if f.Contact != nil {
		t.Fatalf("ambiguous mailto picked: %+v", f.Contact)
	}
	// oebb.at / bosch.com: an address object with only a country is a missing address.
	f = &Fields{CompanyName: s("x"), Address: &Address{Country: s("AT")}}
	if ws := Validate(f, "x"); !hasWarning(ws, "postal address not found") {
		t.Fatalf("country-only address: %+v", ws)
	}
}

func TestCloudflareProtectedEmail(t *testing.T) {
	text := "BLACKROLL AG Email: [\\[email protected\\]](https://blackroll.com/cdn-cgi/l/email-protection#780b101708561c1d381a14191b130a171414561b1715)"
	f := &Fields{CompanyName: s("BLACKROLL AG"), Address: &Address{City: s("x")}, Contact: &Contact{Email: s("[email protected]")}}
	Validate(f, text)
	if f.Contact == nil || f.Contact.Email == nil || *f.Contact.Email != "shop.de@blackroll.com" {
		t.Fatalf("contact = %+v", f.Contact)
	}
}

func TestPhoneGroundingIgnoresPrefixSpelling(t *testing.T) {
	text := "Porsche AG Tel: (+49) 0711 911-0 Fax +41 (0)71 588 02 44"
	for _, v := range []string{"+49 711 911-0", "(+49) 0711 911-0", "0711 9110", "+41 71 588 02 44"} {
		if !phoneGrounded(text, v) {
			t.Errorf("%q should be grounded in %q", v, text)
		}
	}
	for _, v := range []string{"+49 711 911-1", "0800 123456"} {
		if phoneGrounded(text, v) {
			t.Errorf("%q must not be grounded", v)
		}
	}
}
