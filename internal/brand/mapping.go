package brand

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const maxMapEntries = 20

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// first returns the first non-nil pointer.
func first(ps ...*string) *string {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// object returns m[key] as a map, or nil.
func object(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

// str returns m[key] as a trimmed non-empty string, or nil.
func str(m map[string]any, key string) *string {
	s, ok := m[key].(string)
	if !ok {
		return nil
	}
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

func clip(s *string, n int) *string {
	if s == nil || len(*s) <= n {
		return s
	}
	r := []rune(*s)
	if len(r) <= n {
		return s
	}
	return ptr(string(r[:n]))
}

var reHex = regexp.MustCompile(`^#(?:[0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$`)

// hexColor returns a lowercase #rrggbb(aa) color; #rgb is expanded. Anything
// else (rgb(), named colors, garbage) is dropped rather than guessed.
func hexColor(s *string) *string {
	if s == nil {
		return nil
	}
	h := strings.ToLower(*s)
	if !reHex.MatchString(h) {
		return nil
	}
	if len(h) == 4 {
		h = "#" + strings.Repeat(string(h[1]), 2) + strings.Repeat(string(h[2]), 2) + strings.Repeat(string(h[3]), 2)
	}
	return &h
}

// maxDataURI bounds inline images. Sites often inline their SVG logo as a data URI
// (observed on the real API), which is useful, but a data URI can be megabytes.
const maxDataURI = 32 << 10

var reSVGActive = regexp.MustCompile(`(?i)<\s*(script|foreignobject|iframe|embed|object|handler)|\bon[a-z]+\s*=|javascript:|@import|<!entity`)

// activeSVG reports whether an SVG data URI carries active content (scripts, event handlers,
// embedded documents). Such logos are dropped: a caller that renders the logo with something
// other than <img> would otherwise execute third-party script. Other image types pass.
func activeSVG(dataURI string) bool {
	meta, payload, ok := strings.Cut(dataURI, ",")
	if !ok {
		return true
	}
	if !strings.Contains(strings.ToLower(meta), "svg") {
		return false
	}
	var body string
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return true
		}
		body = string(b)
	} else {
		u, err := url.PathUnescape(payload)
		if err != nil {
			return true
		}
		body = u
	}
	return reSVGActive.MatchString(body)
}

// assetURL resolves s against base and keeps only http(s) URLs and small
// data:image/ URIs (never javascript:, data:text/html and the like).
func assetURL(base *url.URL, s *string) *string {
	if s == nil {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(*s), "data:") {
		if strings.HasPrefix(strings.ToLower(*s), "data:image/") && len(*s) <= maxDataURI && !activeSVG(*s) {
			return s
		}
		return nil
	}
	u, err := url.Parse(*s)
	if err != nil {
		return nil
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	out := u.String()
	return &out
}

var reLang = regexp.MustCompile(`^[a-z]{2,3}(?:[-_][a-z]{2,4})?$`)

// language reduces a language tag to its primary subtag ("de-AT" -> "de").
func language(s *string) *string {
	if s == nil {
		return nil
	}
	l := strings.ToLower(*s)
	if !reLang.MatchString(l) {
		return nil
	}
	l, _, _ = strings.Cut(strings.ReplaceAll(l, "_", "-"), "-")
	return &l
}

// keywords normalizes a string or list of strings into at most five short lowercase keywords.
func keywords(v any) []string {
	var raw []string
	switch x := v.(type) {
	case string:
		raw = strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ';' || r == '/' })
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || len([]rune(k)) > 30 || slices.Contains(out, k) {
			continue
		}
		out = append(out, k)
		if len(out) == 5 {
			break
		}
	}
	return out
}

// mapTypography fills fonts and typography from branding.typography and branding.fonts.
func mapTypography(b *Brand, typo map[string]any, fonts any) {
	families := object(typo, "fontFamilies")
	for _, role := range []string{"primary", "heading", "code"} {
		if f := str(families, role); f != nil {
			b.Typography.FontFamilies[role] = *f
			b.addFont(*f, role)
		}
	}
	if list, ok := fonts.([]any); ok {
		for _, e := range list {
			switch x := e.(type) {
			case string:
				b.addFont(x, "")
			case map[string]any:
				if f := str(x, "family"); f != nil {
					b.addFont(*f, deref(str(x, "role")))
				}
			}
		}
	}
	for k, v := range object(typo, "fontSizes") {
		if len(b.Typography.FontSizes) >= maxMapEntries {
			break
		}
		switch x := v.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				b.Typography.FontSizes[k] = s
			}
		case float64:
			b.Typography.FontSizes[k] = strconv.FormatFloat(x, 'f', -1, 64)
		}
	}
	for k, v := range object(typo, "fontWeights") {
		if len(b.Typography.FontWeights) >= maxMapEntries {
			break
		}
		switch x := v.(type) {
		case float64:
			b.Typography.FontWeights[k] = int(x)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
				b.Typography.FontWeights[k] = n
			}
		}
	}
}

func (b *Brand) addFont(family, role string) {
	family = strings.TrimSpace(family)
	if family == "" || len(family) > 200 {
		return
	}
	f := Font{Family: family, Role: role}
	if !slices.Contains(b.Fonts, f) && len(b.Fonts) < maxMapEntries {
		b.Fonts = append(b.Fonts, f)
	}
}
