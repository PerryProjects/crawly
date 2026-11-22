package imprint

import (
	"encoding/json"
	"strconv"
	"strings"
)

// flexString accepts a JSON string or number and ignores everything else, so a
// sloppy LLM answer degrades to "absent" instead of failing the whole decode.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil //nolint:nilerr // tolerant by design
	}
	switch x := v.(type) {
	case string:
		*f = flexString(x)
	case float64:
		*f = flexString(strconv.FormatFloat(x, 'f', -1, 64))
	}
	return nil
}

// flexList accepts a string (split on newlines, ';' and ',') or an array of strings.
type flexList []string

func (f *flexList) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil //nolint:nilerr // tolerant by design
	}
	switch x := v.(type) {
	case string:
		*f = strings.FieldsFunc(x, func(r rune) bool { return r == '\n' || r == ';' || r == ',' })
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				*f = append(*f, s)
			}
		}
	}
	return nil
}

type rawAddress struct {
	Street     *flexString `json:"street"`
	PostalCode *flexString `json:"postal_code"`
	City       *flexString `json:"city"`
	Country    *flexString `json:"country"`
}

type rawContact struct {
	Email *flexString `json:"email"`
	Phone *flexString `json:"phone"`
	Fax   *flexString `json:"fax"`
}

type rawRegister struct {
	Type   *flexString `json:"type"`
	Court  *flexString `json:"court"`
	Number *flexString `json:"number"`
}

// field decodes m[key] into T. A missing or wrongly shaped value yields nil, so
// one bad field never discards the others.
func field[T any](m map[string]any, key string) *T {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out T
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return &out
}

// placeholders are LLM stand-ins for "nothing there".
var placeholders = map[string]bool{
	"": true, "null": true, "none": true, "n/a": true, "na": true, "-": true, "–": true, "—": true,
	"unknown": true, "unbekannt": true, "nicht angegeben": true, "k.a.": true, "keine angabe": true,
}

// clean trims s and maps empty values and placeholders to nil.
func clean(s *flexString) *string {
	if s == nil {
		return nil
	}
	v := strings.Join(strings.Fields(string(*s)), " ")
	if placeholders[strings.ToLower(v)] {
		return nil
	}
	return &v
}

// FieldsFromMap converts the loosely typed Firecrawl JSON result into Fields.
// Unknown keys are ignored; wrongly typed values become nil.
func FieldsFromMap(m map[string]any) Fields {
	str := func(key string) *string {
		if p := field[flexString](m, key); p != nil {
			return clean(p)
		}
		return nil
	}
	f := Fields{
		CompanyName:           str("company_name"),
		LegalForm:             str("legal_form"),
		VATID:                 str("vat_id"),
		SupervisoryAuthority:  str("supervisory_authority"),
		ResponsibleForContent: str("responsible_for_content"),
		Country:               str("country"),
		RepresentedBy:         []string{},
	}
	if l := field[flexList](m, "represented_by"); l != nil {
		for _, p := range *l {
			if v := clean(ptr(flexString(p))); v != nil {
				f.RepresentedBy = append(f.RepresentedBy, *v)
			}
		}
	}
	if a := field[rawAddress](m, "address"); a != nil {
		f.Address = nonEmpty(&Address{Street: clean(a.Street), PostalCode: clean(a.PostalCode), City: clean(a.City), Country: clean(a.Country)})
	}
	if c := field[rawContact](m, "contact"); c != nil {
		f.Contact = nonEmpty(&Contact{Email: clean(c.Email), Phone: clean(c.Phone), Fax: clean(c.Fax)})
	}
	if g := field[rawRegister](m, "register"); g != nil {
		f.Register = nonEmpty(&Register{Type: clean(g.Type), Court: clean(g.Court), Number: clean(g.Number)})
	}
	return f
}

func ptr[T any](v T) *T { return &v }

// nonEmpty returns nil when every field of the struct is nil.
func nonEmpty[T Address | Contact | Register](v *T) *T {
	var zero T
	if *v == zero {
		return nil
	}
	return v
}

// normalizeCountry maps common spellings to DE, AT or CH. It returns "" for anything else.
func normalizeCountry(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "de", "deu", "germany", "deutschland", "bundesrepublik deutschland":
		return "DE"
	case "at", "aut", "austria", "österreich", "oesterreich":
		return "AT"
	case "ch", "che", "switzerland", "schweiz", "suisse", "svizzera", "swiss confederation":
		return "CH"
	}
	return ""
}
