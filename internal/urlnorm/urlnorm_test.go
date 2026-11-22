package urlnorm

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantURL  string
	}{
		{"https://example.de", "example.de", "https://example.de"},
		{"HTTPS://WWW.Example.DE/Impressum", "example.de", "https://example.de/Impressum"},
		{"example.de", "example.de", "https://example.de"},
		{"  http://example.at:80/x  ", "example.at", "http://example.at/x"},
		{"https://example.ch:443/", "example.ch", "https://example.ch/"},
		{"https://example.de./", "example.de", "https://example.de/"},
		{"https://bücher.de/", "xn--bcher-kva.de", "https://xn--bcher-kva.de/"},
		{"https://www.münchen.de", "xn--mnchen-3ya.de", "https://xn--mnchen-3ya.de"},
		{"https://xn--bcher-kva.de", "xn--bcher-kva.de", "https://xn--bcher-kva.de"},
		{"https://shop.example.de/a?utm_source=x&id=7&fbclid=abc", "shop.example.de", "https://shop.example.de/a?id=7"},
		{"https://example.de/?UTM_Campaign=a&gclid=1", "example.de", "https://example.de/"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Host != tt.wantHost || got.URL != tt.wantURL {
				t.Errorf("got %+v, want host=%s url=%s", got, tt.wantHost, tt.wantURL)
			}
			if got.Homepage() != "https://"+tt.wantHost+"/" {
				t.Errorf("homepage = %s", got.Homepage())
			}
		})
	}
}

func TestNormalizeRejects(t *testing.T) {
	tests := map[string]string{
		"empty":             "",
		"spaces only":       "   ",
		"ftp":               "ftp://example.de",
		"javascript":        "javascript:alert(1)",
		"mailto":            "mailto:a@example.de",
		"file":              "file:///etc/passwd",
		"userinfo":          "https://user:pw@example.de",
		"userinfo only":     "https://user@example.de",
		"fragment":          "https://example.de/#top",
		"empty fragment":    "https://example.de/#",
		"port":              "https://example.de:8443",
		"http on 443":       "http://example.de:443",
		"https on 80":       "https://example.de:80",
		"localhost":         "http://localhost",
		"localhost suffix":  "http://app.localhost",
		"local":             "https://printer.local",
		"internal":          "https://db.internal",
		"lan":               "https://nas.lan",
		"ipv4":              "https://192.168.0.1",
		"ipv4 public":       "https://8.8.8.8",
		"ipv6":              "https://[::1]/",
		"ipv6 mapped":       "https://[::ffff:127.0.0.1]/",
		"decimal ip":        "https://2130706433",
		"short ip":          "https://127.1",
		"hex ip":            "https://0x7f.0x0.0x0.0x1",
		"single label":      "https://intranet",
		"empty host":        "https:///path",
		"bad idna":          "https://exa mple.de",
		"underscore label":  "https://-bad-.de",
		"too long":          "https://example.de/" + strings.Repeat("a", maxInputLen),
		"home.arpa":         "https://router.home.arpa",
		"scheme only":       "https://",
		"www plus tld only": "https://www.de/",
		"control character": "https://exa\x00mple.de",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(in)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("Normalize(%q) = %+v, %v; want ErrInvalidURL", in, got, err)
			}
		})
	}
}
