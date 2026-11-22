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
	// Spaced spellings occur too ("HR B 59603", seen live).
	// Also "HRB-Nr. 730623" (seen live on porsche.com).
	reRegPrefixed = regexp.MustCompile(`(?i)^(H\s?R\s?A|H\s?R\s?B|G\s?n\s?R|P\s?R|V\s?R|F\s?N)[-.:\s]*(?:Nr\.?)?[.:\s]*(\d.*)$`)
	reMailto      = regexp.MustCompile(`(?i)mailto:([^)\s?"'>]+)`)
	// Separators between several VAT IDs in one field ("DE 326114858, CHE-276.969.644", seen live).
	reVATSplit = regexp.MustCompile(`(?i)\s*(?:[,;/|]|\bund\b|\band\b)\s*`)

	reCHUID    = regexp.MustCompile(`^CHE-\d{3}\.\d{3}\.\d{3}$`)
	reDigitGap = regexp.MustCompile(`(\d)[ .](\d)`)

	rePostalDE = regexp.MustCompile(`^\d{5}$`)
	rePostal4  = regexp.MustCompile(`^\d{4}$`) // AT and CH
)

// registerTypeInText finds "<type> <number>" in text, e.g. "HRB 382746" or "HR B 382746", and
// returns the canonical type, or "".
func registerTypeInText(text, number string) string {
	digits := strings.Join(strings.Fields(number), `\s*`)
	re, err := regexp.Compile(`(?i)\b(H\s?R\s?A|H\s?R\s?B|G\s?n\s?R|P\s?R|V\s?R|F\s?N)\.?\s*(?:Nr\.?\s*)?` + digits + `\b`)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	if t, ok := registerTypes[strings.ToUpper(strings.Join(strings.Fields(m[1]), ""))]; ok {
		return t.canonical
	}
	return ""
}

// genericRegisterNames are register names that are not a register type.
var genericRegisterNames = map[string]bool{"HANDELSREGISTER": true, "COMMERCIALREGISTER": true, "HR": true, "REGISTERGERICHT": true}

// registerTypes maps a normalized register type to its canonical spelling and country.
var registerTypes = map[string]struct{ canonical, country string }{
	"HRA": {"HRA", "DE"}, "HRB": {"HRB", "DE"}, "GNR": {"GnR", "DE"}, "PR": {"PR", "DE"}, "VR": {"VR", "DE"},
	"FN": {"FN", "AT"},
}

// Validate normalizes a few fields in place (VAT ID whitespace, register type
// spelling, country code) and returns warnings. text is the raw page text used
// for the grounding check: values the model reports but that do not occur in
// the page are flagged as possible hallucinations.
func Validate(f *Fields, text string) []Warning {
	var w []Warning
	warn := func(code, field, format string, args ...any) {
		w = append(w, Warning{Code: code, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	// Country: normalize to DE/AT/CH; anything else is flagged and dropped.
	if f.Country != nil {
		if c := normalizeCountry(*f.Country); c != "" {
			f.Country = &c
		} else {
			warn(WarnNotDACH, "country", "country %q is not DE, AT or CH; this service only covers DACH", *f.Country)
			f.Country = nil
		}
	}
	country := effectiveCountry(f)
	if f.LegalForm != nil {
		if canon, ok := legalForms[strings.ToLower(*f.LegalForm)]; ok && canon != *f.LegalForm {
			f.LegalForm = &canon
		}
	}

	if f.CompanyName == nil {
		warn(WarnMissing, "company_name", "company_name not found")
	} else if !grounded(text, *f.CompanyName) {
		warn(WarnUnverified, "company_name", "company_name not found verbatim in the imprint text")
	}
	if f.Address == nil || f.Address.Street == nil && f.Address.PostalCode == nil && f.Address.City == nil {
		warn(WarnMissing, "address", "postal address not found")
	}

	validateVAT(f, text, country, warn)
	validateRegister(f, text, country, warn)

	if f.Contact != nil {
		if e := f.Contact.Email; e != nil {
			// Grounding is checked on the value as written on the page, before any normalization.
			plain, deobfuscated := deobfuscateEmail(*e)
			switch {
			case !grounded(text, *e):
				warn(WarnUnverifiedRemoved, "contact.email", "contact.email not found verbatim in the imprint text, value removed")
				f.Contact.Email = nil
			case validEmail(*e):
			case deobfuscated && validEmail(plain):
				warn(WarnNormalized, "contact.email", "contact.email was written obfuscated on the page (%q) and has been normalized", *e)
				f.Contact.Email = &plain
			default:
				warn(WarnInvalidRemoved, "contact.email", "contact.email %q is not a valid single email address, value removed", *e)
				f.Contact.Email = nil
			}
		}
		dropUngrounded(text, "contact.phone", &f.Contact.Phone, warn)
		dropUngrounded(text, "contact.fax", &f.Contact.Fax, warn)
		f.Contact = nonEmpty(f.Contact)
	}
	// Cloudflare hides addresses as "[email protected]" (seen live on blackroll.com) while the page still
	// carries the mailto link. With no usable address, take it when the page links exactly one.
	if f.Contact == nil || f.Contact.Email == nil {
		if addr, ok := onlyMailto(text); ok {
			if f.Contact == nil {
				f.Contact = &Contact{}
			}
			f.Contact.Email = &addr
			warn(WarnNormalized, "contact.email", "contact.email taken from the only mailto link on the page")
		}
	}
	if f.Address != nil && f.Address.PostalCode != nil {
		pc := *f.Address.PostalCode
		switch {
		case country == "DE" && !rePostalDE.MatchString(pc):
			warn(WarnInvalidFormat, "address.postal_code", "address.postal_code %q is not a 5-digit German postal code", pc)
		case (country == "AT" || country == "CH") && !rePostal4.MatchString(pc):
			warn(WarnInvalidFormat, "address.postal_code", "address.postal_code %q is not a 4-digit %s postal code", pc, country)
		case !grounded(text, pc):
			warn(WarnUnverified, "address.postal_code", "address.postal_code not found in the imprint text")
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

// classifyVAT normalizes one VAT ID and returns its country, or ok=false if it matches no format.
func classifyVAT(raw string) (v, vatCountry string, ok bool) {
	v = compact(raw)
	switch {
	case reVATDE.MatchString(v):
		return v, "DE", true
	case reVATAT.MatchString(v):
		return v, "AT", true
	case reVATCH.MatchString(v):
		if m := reVATCH.FindStringSubmatch(v); m[1] != "" {
			v = strings.TrimSuffix(v, m[1]) + " " + m[1]
		}
		return v, "CH", true
	}
	return v, "", false
}

func validateVAT(f *Fields, text, country string, warn func(string, string, string, ...any)) {
	if f.VATID == nil {
		return
	}
	v, vatCountry, ok := classifyVAT(*f.VATID)
	if !ok {
		// Several IDs in one field: keep the one of the imprint country (else the first) in vat_id.
		var valid [][2]string
		for _, part := range reVATSplit.Split(*f.VATID, -1) {
			if pv, pc, pok := classifyVAT(part); pok && grounded(text, pv) {
				valid = append(valid, [2]string{pv, pc})
			}
		}
		if len(valid) < 2 {
			warn(WarnInvalidFormat, "vat_id", "vat_id %q does not match the DE (DE+9 digits), AT (ATU+8 digits) or CH (CHE-123.456.789) format", *f.VATID)
			return
		}
		primary := 0
		for i, x := range valid {
			if x[1] == country {
				primary = i
				break
			}
		}
		v, vatCountry = valid[primary][0], valid[primary][1]
		for i, x := range valid {
			if i != primary {
				f.OtherVATIDs = append(f.OtherVATIDs, x[0])
			}
		}
		warn(WarnNormalized, "vat_id", "several VAT IDs were listed (%q); vat_id holds %s, the others are in other_vat_ids", *f.VATID, v)
	}
	f.VATID = &v
	if country != "" && vatCountry != country {
		warn(WarnCountryMismatch, "vat_id", "vat_id has a %s format but the imprint country is %s", vatCountry, country)
	}
	if !grounded(text, v) {
		warn(WarnUnverifiedRemoved, "vat_id", "vat_id not found verbatim in the imprint text, value removed")
		f.VATID = nil
	}
}

// dropUngrounded removes identifiers that the model reports but that do not occur in the
// page. Prompt rule: these are copied exactly, so a value that is not on the page was
// invented (observed live: an e-mail address composed from the shop's domain).
func dropUngrounded(text, name string, v **string, warn func(string, string, string, ...any)) {
	if *v != nil && !grounded(text, **v) {
		warn(WarnUnverifiedRemoved, name, "%s not found verbatim in the imprint text, value removed", name)
		*v = nil
	}
}

func validateRegister(f *Fields, text, country string, warn func(string, string, string, ...any)) {
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
				t := strings.Join(strings.Fields(m[1]), "")
				r.Type = &t
			}
		}
	}
	// The model sometimes keeps only "HR" or nothing while the page says "HRB 382746" (seen live:
	// bergfreunde.de). Take the type from the page when it stands right before this number.
	if r.Number != nil && (r.Type == nil || genericRegisterNames[strings.ToUpper(strings.ReplaceAll(*r.Type, " ", ""))]) {
		if t := registerTypeInText(text, *r.Number); t != "" {
			r.Type = &t
			warn(WarnNormalized, "register.type", "register.type %s taken from the page text next to the number", t)
		}
	}
	// "Firmenbuchnummer: 42354k" (seen live): the page names the Austrian Firmenbuch, the number has
	// its format, only the abbreviation is missing.
	if r.Type == nil && r.Number != nil && reRegAT.MatchString(strings.ToUpper(strings.TrimSpace(*r.Number))) &&
		strings.Contains(strings.ToLower(text), "firmenbuch") {
		fn := "FN"
		r.Type = &fn
		warn(WarnNormalized, "register.type", "register.type set to FN: the page names the Firmenbuch and the number has its format")
	}
	// Swiss companies are registered under their UID (seen live: blackroll.com lists only "CHE-228.574.355").
	if r.Type == nil && r.Number != nil && reCHUID.MatchString(strings.TrimSpace(*r.Number)) {
		uid := "UID"
		r.Type = &uid
		warn(WarnNormalized, "register.type", "register.type set to UID: the number is a Swiss company identification number")
	}
	var typ struct{ canonical, country string }
	known := false
	if r.Type != nil {
		key := strings.ToUpper(strings.NewReplacer(".", "", " ", "", "-", "").Replace(*r.Type))
		if typ, known = registerTypes[key]; known {
			c := typ.canonical
			r.Type = &c
		} else if genericRegisterNames[key] && country == "DE" {
			// Seen live: "Entry in Handelsregister, Register Number: 166842" — the page does not say HRA or HRB.
			warn(WarnMissing, "register.type", "register.type only names the register (%q); the page does not state the type (e.g. HRA or HRB)", *r.Type)
		} else if country == "DE" || country == "AT" {
			warn(WarnInvalidFormat, "register.type", "register.type %q is not a known %s register type", *r.Type, country)
		}
	}
	if known && country != "" && country != "CH" && typ.country != country {
		warn(WarnCountryMismatch, "register.type", "register.type %s belongs to %s but the imprint country is %s", typ.canonical, typ.country, country)
	}
	if r.Number == nil {
		return
	}
	num := strings.ToUpper(strings.TrimSpace(*r.Number))
	num = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(num, "NR.")), "NR")
	switch {
	case known && typ.country == "AT", !known && country == "AT":
		if !reRegAT.MatchString(strings.TrimSpace(num)) {
			warn(WarnInvalidFormat, "register.number", "register.number %q is not a valid Firmenbuch number (digits plus a letter, e.g. 123456a)", *r.Number)
		}
	case known && typ.country == "DE", !known && country == "DE":
		// Digit groups are sometimes spaced ("HRA 6 11 12", seen live); judge the format without them.
		if !reRegDE.MatchString(reDigitGap.ReplaceAllString(strings.TrimSpace(num), "$1$2")) {
			warn(WarnInvalidFormat, "register.number", "register.number %q is not a valid German register number", *r.Number)
		}
	}
	if !known && r.Type == nil {
		warn(WarnMissing, "register.type", "register.number present without register.type")
	}
	dropUngrounded(text, "register.number", &r.Number, warn)
	if *r == (Register{}) {
		f.Register = nil
	}
}

var (
	reObfAt  = regexp.MustCompile(`(?i)\s*[\[({<]\s*(?:at|ät|@)\s*[\])}>]\s*`)
	reObfDot = regexp.MustCompile(`(?i)\s*[\[({<]\s*(?:dot|punkt|\.)\s*[\])}>]\s*`)
)

// deobfuscateEmail turns the common anti-spam spellings "name [at] example [dot] de" and
// "name (at) example.de" into a plain address. Only bracketed tokens are replaced, never a bare
// "at", so ordinary text cannot be turned into an address. It reports whether anything changed.
func deobfuscateEmail(s string) (string, bool) {
	out := reObfDot.ReplaceAllString(reObfAt.ReplaceAllString(s, "@"), ".")
	out = strings.TrimSpace(out)
	return out, out != strings.TrimSpace(s)
}

// legalForms maps the lower-cased spelling of common DACH legal forms to their canonical
// spelling. Only the letter case is changed, and only for exact matches.
var legalForms = func() map[string]string {
	m := map[string]string{}
	for _, f := range []string{
		"GmbH", "gGmbH", "UG", "UG (haftungsbeschränkt)", "AG", "SE", "KG", "OHG", "GbR", "KGaA", "eG", "e.K.", "e.V.",
		"GmbH & Co. KG", "AG & Co. KG", "UG (haftungsbeschränkt) & Co. KG", "PartG", "PartG mbB", "Einzelunternehmen",
		"e.U.", "OG", "SA", "Sàrl", "Sagl", "Genossenschaft", "Stiftung",
	} {
		m[strings.ToLower(f)] = f
	}
	return m
}()

// onlyMailto returns the address when the text links exactly one distinct, valid mailto address.
func onlyMailto(text string) (string, bool) {
	var found string
	for _, m := range reMailto.FindAllStringSubmatch(text, -1) {
		a := strings.ToLower(m[1])
		if !validEmail(a) {
			continue
		}
		if found != "" && found != a {
			return "", false
		}
		found = a
	}
	return found, found != ""
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
