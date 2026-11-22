package imprint

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
)

// Deterministic validation. Every rule only produces a warning, never an error,
// and is deliberately conservative: it flags values that are clearly malformed
// for the identified country and stays silent about anything it cannot judge.
// No checksum is verified (DE VAT checksums are not checked) and nothing is
// looked up in a registry.
var (
	// DE VAT: "DE" + 9 digits.
	reVATDE = regexp.MustCompile(`^DE\d{9}$`)
	// AT VAT: "ATU" + 8 digits.
	reVATAT = regexp.MustCompile(`^ATU\d{8}$`)
	// CH UID: "CHE-123.456.789", optionally followed by the VAT suffix MWST/TVA/IVA.
	reVATCH = regexp.MustCompile(`^CHE-\d{3}\.\d{3}\.\d{3}(MWST|TVA|IVA)?$`)
	// DE register number: up to 7 digits, optional trailing letters (e.g. "12345 B", "123456 FL").
	reRegDE = regexp.MustCompile(`^\d{1,7}(?: ?[A-Z]{1,3})?$`)
	// AT Firmenbuch number: digits plus one check letter (e.g. "123456a").
	reRegAT = regexp.MustCompile(`^\d{1,7} ?[A-Z]$`)

	// A register type glued to the number ("HRB 104144", "FN123456a"), a common LLM habit.
	reRegPrefixed = regexp.MustCompile(`(?i)^(HRA|HRB|GNR|PR|VR|FN)[.:\s]*(\d.*)$`)

	rePostalDE = regexp.MustCompile(`^\d{5}$`)
	rePostal4  = regexp.MustCompile(`^\d{4}$`) // AT and CH
)

// registerTypes maps a normalized register type to its canonical spelling and country.
var registerTypes = map[string]struct{ canonical, country string }{
	"HRA": {"HRA", "DE"}, "HRB": {"HRB", "DE"}, "GNR": {"GnR", "DE"}, "PR": {"PR", "DE"}, "VR": {"VR", "DE"},
	"FN": {"FN", "AT"},
}

// Validate normalizes a few fields in place (VAT ID whitespace, register type
// spelling, country code) and returns warnings. text is the raw page text used
// for the grounding check: values the model reports but that do not occur in
// the page are flagged as possible hallucinations.
func Validate(f *Fields, text string) []string {
	var w []string
	warn := func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }

	// Country: normalize to DE/AT/CH; anything else is flagged and dropped.
	if f.Country != nil {
		if c := normalizeCountry(*f.Country); c != "" {
			f.Country = &c
		} else {
			warn("country %q is not DE, AT or CH; this service only covers DACH", *f.Country)
			f.Country = nil
		}
	}
	country := effectiveCountry(f)

	if f.CompanyName == nil {
		warn("company_name not found")
	} else if !grounded(text, *f.CompanyName) {
		warn("company_name not found verbatim in the imprint text")
	}
	if f.Address == nil {
		warn("postal address not found")
	}

	validateVAT(f, text, country, warn)
	validateRegister(f, text, country, warn)

	if f.Contact != nil {
		if f.Contact.Email != nil {
			if !validEmail(*f.Contact.Email) {
				warn("contact.email %q is not a valid single email address, value removed", *f.Contact.Email)
				f.Contact.Email = nil
			} else if !grounded(text, *f.Contact.Email) {
				warn("contact.email not found verbatim in the imprint text, value removed")
				f.Contact.Email = nil
			}
		}
		dropUngrounded(text, "contact.phone", &f.Contact.Phone, warn)
		dropUngrounded(text, "contact.fax", &f.Contact.Fax, warn)
		f.Contact = nonEmpty(f.Contact)
	}
	if f.Address != nil && f.Address.PostalCode != nil {
		pc := *f.Address.PostalCode
		switch {
		case country == "DE" && !rePostalDE.MatchString(pc):
			warn("address.postal_code %q is not a 5-digit German postal code", pc)
		case (country == "AT" || country == "CH") && !rePostal4.MatchString(pc):
			warn("address.postal_code %q is not a 4-digit %s postal code", pc, country)
		case !grounded(text, pc):
			warn("address.postal_code not found in the imprint text")
		}
	}
	return w
}

func effectiveCountry(f *Fields) string {
	if f.Country != nil {
		return *f.Country
	}
	if f.Address != nil && f.Address.Country != nil {
		if c := normalizeCountry(*f.Address.Country); c != "" {
			return c
		}
	}
	if f.VATID != nil {
		v := compact(*f.VATID)
		switch {
		case strings.HasPrefix(v, "DE"):
			return "DE"
		case strings.HasPrefix(v, "ATU"):
			return "AT"
		case strings.HasPrefix(v, "CHE"):
			return "CH"
		}
	}
	return ""
}

func validateVAT(f *Fields, text, country string, warn func(string, ...any)) {
	if f.VATID == nil {
		return
	}
	v := compact(*f.VATID)
	var vatCountry string
	switch {
	case reVATDE.MatchString(v):
		vatCountry = "DE"
	case reVATAT.MatchString(v):
		vatCountry = "AT"
	case reVATCH.MatchString(v):
		vatCountry = "CH"
		if m := reVATCH.FindStringSubmatch(v); m[1] != "" {
			v = strings.TrimSuffix(v, m[1]) + " " + m[1]
		}
	default:
		warn("vat_id %q does not match the DE (DE+9 digits), AT (ATU+8 digits) or CH (CHE-123.456.789) format", *f.VATID)
		return
	}
	f.VATID = &v
	if country != "" && vatCountry != country {
		warn("vat_id has a %s format but the imprint country is %s", vatCountry, country)
	}
	if !grounded(text, v) {
		warn("vat_id not found verbatim in the imprint text, value removed")
		f.VATID = nil
	}
}

// dropUngrounded removes identifiers that the model reports but that do not occur in the
// page. Prompt rule: these are copied exactly, so a value that is not on the page was
// invented (observed live: an e-mail address composed from the shop's domain).
func dropUngrounded(text, name string, v **string, warn func(string, ...any)) {
	if *v != nil && !grounded(text, **v) {
		warn("%s not found verbatim in the imprint text, value removed", name)
		*v = nil
	}
}

func validateRegister(f *Fields, text, country string, warn func(string, ...any)) {
	r := f.Register
	if r == nil {
		return
	}
	// Split "hrb 104144" in the number field into type and number (only whitespace and case change).
	if r.Number != nil {
		if m := reRegPrefixed.FindStringSubmatch(strings.TrimSpace(*r.Number)); m != nil {
			num := strings.TrimSpace(m[2])
			r.Number = &num
			if r.Type == nil {
				t := m[1]
				r.Type = &t
			}
		}
	}
	var typ struct{ canonical, country string }
	known := false
	if r.Type != nil {
		key := strings.ToUpper(strings.NewReplacer(".", "", " ", "", "-", "").Replace(*r.Type))
		if typ, known = registerTypes[key]; known {
			c := typ.canonical
			r.Type = &c
		} else if country == "DE" || country == "AT" {
			warn("register.type %q is not a known %s register type", *r.Type, country)
		}
	}
	if known && country != "" && country != "CH" && typ.country != country {
		warn("register.type %s belongs to %s but the imprint country is %s", typ.canonical, typ.country, country)
	}
	if r.Number == nil {
		return
	}
	num := strings.ToUpper(strings.TrimSpace(*r.Number))
	num = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(num, "NR.")), "NR")
	switch {
	case known && typ.country == "AT", !known && country == "AT":
		if !reRegAT.MatchString(strings.TrimSpace(num)) {
			warn("register.number %q is not a valid Firmenbuch number (digits plus a letter, e.g. 123456a)", *r.Number)
		}
	case known && typ.country == "DE", !known && country == "DE":
		if !reRegDE.MatchString(strings.TrimSpace(num)) {
			warn("register.number %q is not a valid German register number", *r.Number)
		}
	}
	if !known && r.Type == nil {
		warn("register.number present without register.type")
	}
	dropUngrounded(text, "register.number", &r.Number, warn)
	if *r == (Register{}) {
		f.Register = nil
	}
}

// validEmail accepts exactly one plain address (no display name) with a dotted domain.
func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return false
	}
	_, domain, ok := strings.Cut(a.Address, "@")
	return ok && strings.Contains(domain, ".") && !strings.HasSuffix(domain, ".")
}

// compact removes all whitespace and upper-cases (VAT IDs are written with spaces, dots vary).
func compact(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == ' ' {
			return -1
		}
		return r
	}, s))
}

// squash keeps only lower-cased letters and digits, so "DE 123.456 789" and
// "de123456789" compare equal and markdown punctuation does not matter.
func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// grounded reports whether value occurs in text, ignoring case, whitespace and punctuation.
// An empty text (nothing to compare against) counts as grounded.
func grounded(text, value string) bool {
	if text == "" {
		return true
	}
	v := squash(value)
	return v == "" || strings.Contains(squash(text), v)
}
