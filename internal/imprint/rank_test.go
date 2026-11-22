package imprint

import (
	"net/url"
	"slices"
	"testing"
)

func urls(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.URL
	}
	return out
}

func TestRankLinks(t *testing.T) {
	base, _ := url.Parse("https://www.example.de/")
	hosts := []string{"example.de", "www.example.de"}

	tests := []struct {
		name  string
		links []Link
		want  []string
	}{
		{
			name: "exact /impressum beats deeper imprint-like paths",
			links: []Link{
				{URL: "https://example.de/rechtliches/impressum-und-kontakt", Text: "Impressum"},
				{URL: "https://example.de/impressum", Text: "Impressum"},
				{URL: "https://example.de/imprint", Text: "Imprint"},
			},
			want: []string{"https://example.de/impressum", "https://example.de/imprint", "https://example.de/rechtliches/impressum-und-kontakt"},
		},
		{
			name: "privacy links are excluded",
			links: []Link{
				{URL: "https://example.de/datenschutz", Text: "Datenschutz"},
				{URL: "https://example.de/privacy-policy", Text: "Privacy"},
				{URL: "https://example.de/cookie-einstellungen", Text: "Cookies"},
				{URL: "https://example.de/impressum", Text: "Impressum"},
			},
			want: []string{"https://example.de/impressum"},
		},
		{
			name: "combined page is kept but loses against a pure imprint page",
			links: []Link{
				{URL: "https://example.de/impressum-datenschutz", Text: "Impressum & Datenschutz"},
				{URL: "https://example.de/legal-notice", Text: "Legal notice"},
			},
			want: []string{"https://example.de/legal-notice", "https://example.de/impressum-datenschutz"},
		},
		{
			name:  "link text only match",
			links: []Link{{URL: "https://example.de/info/42", Text: "Rechtliche Hinweise"}, {URL: "https://example.de/about", Text: "Über uns"}},
			want:  []string{"https://example.de/info/42"},
		},
		{
			name: "same host preferred over external, external kept",
			links: []Link{
				{URL: "https://other-hoster.com/impressum/acme", Text: "Impressum"},
				{URL: "https://example.de/de/impressum/", Text: "Impressum"},
			},
			want: []string{"https://example.de/de/impressum/", "https://other-hoster.com/impressum/acme"},
		},
		{
			name: "relative links resolve, fragments and duplicates collapse, non-http dropped",
			links: []Link{
				{URL: "/impressum#top", Text: ""},
				{URL: "https://example.de/impressum/", Text: "Impressum"},
				{URL: "mailto:impressum@example.de", Text: "Impressum"},
				{URL: "tel:+49123", Text: "Impressum"},
				{URL: "#impressum", Text: "Impressum"},
				{URL: "javascript:void(0)", Text: "Impressum"},
			},
			want: []string{"https://www.example.de/impressum"},
		},
		{
			name:  "AT offenlegung and anbieterkennung",
			links: []Link{{URL: "https://example.de/offenlegung", Text: "Offenlegung"}, {URL: "https://example.de/anbieterkennung", Text: ""}},
			want:  []string{"https://example.de/anbieterkennung", "https://example.de/offenlegung"},
		},
		{
			name:  "underscore and space variants",
			links: []Link{{URL: "https://example.de/legal_notice", Text: ""}, {URL: "https://example.de/x", Text: "Legal Notice"}},
			want:  []string{"https://example.de/legal_notice", "https://example.de/x"},
		},
		{name: "nothing relevant", links: []Link{{URL: "https://example.de/produkte", Text: "Produkte"}}, want: []string{}},
		{
			name:  "query string is penalized",
			links: []Link{{URL: "https://example.de/index.php?id=impressum", Text: "Impressum"}, {URL: "https://example.de/impressum.html", Text: "Impressum"}},
			want:  []string{"https://example.de/impressum.html", "https://example.de/index.php?id=impressum"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := urls(RankLinks(hosts, base, tt.links))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

func TestRankLinksFlagsExternalAndSubdomains(t *testing.T) {
	base, _ := url.Parse("https://example.de/")
	got := RankLinks([]string{"example.de"}, base, []Link{
		{URL: "https://shop.example.de/impressum", Text: "Impressum"},
		{URL: "https://evil.com/impressum", Text: "Impressum"},
	})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, c := range got {
		wantExternal := c.URL == "https://evil.com/impressum"
		if c.External != wantExternal {
			t.Errorf("%s External = %v", c.URL, c.External)
		}
	}
}

func TestMarkdownLinksAndMerge(t *testing.T) {
	md := "Footer: [Impressum](https://example.de/impressum) | [Datenschutz](/datenschutz \"Privacy\") | ![logo](https://example.de/logo.png) | [Kontakt](<https://example.de/kontakt>)"
	got := MarkdownLinks(md)
	want := []Link{{"https://example.de/impressum", "Impressum"}, {"/datenschutz", "Datenschutz"}, {"https://example.de/kontakt", "Kontakt"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	merged := MergeLinks([]string{"https://example.de/impressum", "https://example.de/a"}, md)
	if len(merged) != 4 || merged[0].Text != "Impressum" || merged[1].Text != "" {
		t.Fatalf("merged = %v", merged)
	}
}

func TestRankLinksRejectsUnsafeTargets(t *testing.T) {
	base, _ := url.Parse("https://acme.de/")
	got := RankLinks([]string{"acme.de"}, base, []Link{
		{URL: "http://10.0.0.12:8080/impressum", Text: "Impressum"},
		{URL: "http://192.168.1.1/impressum", Text: "Impressum"},
		{URL: "https://intranet.local/impressum", Text: "Impressum"},
		{URL: "https://acme.de:8443/impressum", Text: "Impressum"},
		{URL: "https://user:pw@acme.de/impressum", Text: "Impressum"},
		{URL: "http://[::1]/impressum", Text: "Impressum"},
		{URL: "http://acme.de/impressum", Text: "Impressum"}, // plain http on the default port is fine
	})
	if len(got) != 1 || got[0].URL != "http://acme.de/impressum" {
		t.Fatalf("got %v", urls(got))
	}
}

func TestSameHostBeatsExternalEvenWithBetterPath(t *testing.T) {
	base, _ := url.Parse("https://www.kessberlin.de/")
	got := RankLinks([]string{"kessberlin.de"}, base, []Link{
		{URL: "https://askstella.ai/imprint/", Text: "Impressum"},
		{URL: "https://www.kessberlin.de/policies/legal-notice", Text: "Impressum"},
	})
	if len(got) != 2 || got[0].URL != "https://www.kessberlin.de/policies/legal-notice" || !got[1].External {
		t.Fatalf("got %+v", got)
	}
}
