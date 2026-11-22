package imprint

import (
	"encoding/json"
	"slices"
	"testing"
)

func decode(t *testing.T, raw string) Fields {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return FieldsFromMap(m)
}

func testFieldsFromMap(t *testing.T) {
	f := decode(t, `{
		"company_name": "  ACME   GmbH ",
		"legal_form": "N/A",
		"represented_by": "Max Mustermann; Erika Musterfrau",
		"address": {"street": "Musterstr. 1", "postal_code": 10115, "city": "Berlin", "country": null},
		"contact": {"email": "", "phone": "null", "fax": "-"},
		"register": {"type": "HRB", "court": "Amtsgericht Berlin", "number": "12345 B"},
		"vat_id": "DE123456789",
		"supervisory_authority": "nicht angegeben",
		"unknown_extra": 1,
		"country": "Deutschland"
	}`)
	if f.CompanyName == nil || *f.CompanyName != "ACME GmbH" {
		t.Errorf("company = %v", f.CompanyName)
	}
	if f.LegalForm != nil || f.SupervisoryAuthority != nil {
		t.Error("placeholders must become nil")
	}
	if !slices.Equal(f.RepresentedBy, []string{"Max Mustermann", "Erika Musterfrau"}) {
		t.Errorf("represented_by = %v", f.RepresentedBy)
	}
	if f.Address == nil || *f.Address.PostalCode != "10115" || f.Address.Country != nil {
		t.Errorf("address = %+v", f.Address)
	}
	if f.Contact != nil {
		t.Errorf("all-empty contact must be nil, got %+v", f.Contact)
	}
	if f.Register == nil || *f.Register.Number != "12345 B" {
		t.Errorf("register = %+v", f.Register)
	}
}

func testFieldsFromMapToleratesWrongTypes(t *testing.T) {
	f := decode(t, `{"company_name": ["a"], "address": "Musterstr. 1, Berlin", "represented_by": [1, "Max"], "vat_id": {"x": 1}}`)
	if f.CompanyName != nil || f.Address != nil || f.VATID != nil {
		t.Errorf("wrongly typed values must be nil: %+v", f)
	}
	if !slices.Equal(f.RepresentedBy, []string{"Max"}) {
		t.Errorf("represented_by = %v", f.RepresentedBy)
	}
	if empty := decode(t, `{}`); empty.RepresentedBy == nil {
		t.Error("represented_by must be an empty slice, not nil")
	}
}

func testNormalizeCountry(t *testing.T) {
	for _, tt := range [][2]string{
		{"DE", "DE"}, {"  deutschland\t", "DE"}, {"Austria", "AT"}, {"Österreich", "AT"},
		{"Schweiz", "CH"}, {"suisse", "CH"}, {"FR", ""}, {"", ""},
	} {
		if got := normalizeCountry(tt[0]); got != tt[1] {
			t.Errorf("normalizeCountry(%q) = %q, want %q", tt[0], got, tt[1])
		}
	}
}

func testExtractionSchemaIsJSONSerializable(t *testing.T) {
	b, err := json.Marshal(extractionSchema())
	if err != nil || len(b) < 100 {
		t.Fatalf("schema: %v (%d bytes)", err, len(b))
	}
}

func TestFields(t *testing.T) {
	t.Run("fields from map", testFieldsFromMap)
	t.Run("fields from map tolerates wrong types", testFieldsFromMapToleratesWrongTypes)
	t.Run("normalize country", testNormalizeCountry)
	t.Run("extraction schema is JSON serializable", testExtractionSchemaIsJSONSerializable)
}
