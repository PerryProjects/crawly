// Package urlnorm validates and normalizes user supplied website URLs.
//
// It never resolves DNS: the hostname checks are purely syntactic, so a public
// name that points to a private address is NOT detected here. See the README
// section on SSRF; the fetch itself is done by Firecrawl, not by this service.
package urlnorm

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/idna"
)

// ErrInvalidURL is returned (wrapped) for every rejected input.
var ErrInvalidURL = errors.New("invalid url")

const maxInputLen = 2048

// Result is a normalized website reference.
type Result struct {
	// Host is the lowercase punycode host without a leading "www.". It is the cache key.
	Host string
	// URL is the canonical form of the input without tracking parameters.
	URL string
}

// Homepage is where discovery always starts.
func (r Result) Homepage() string { return "https://" + r.Host + "/" }

// internalSuffixes are rejected because they are never public DNS names.
var internalSuffixes = []string{
	".local", ".internal", ".lan", ".localhost", ".localdomain", ".home.arpa", ".intranet", ".corp", ".home",
}

// trackingParams are dropped from the canonical URL. Prefix matches are listed in trackingPrefixes.
var trackingParams = []string{"fbclid", "gclid", "dclid", "msclkid", "yclid", "mc_cid", "mc_eid", "_ga", "_gl", "igshid", "ref_src"}

var trackingPrefixes = []string{"utm_", "pk_", "mtm_", "hsa_"}

var reScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// SafeLink reports whether a URL found on a crawled page may be fetched: http(s), no userinfo,
// default port, and a host that passes the same checks as user input (no IP literal, no
// internal name). Discovered links come from untrusted pages, so they get the same SSRF
// screening; as with user input, DNS is not resolved here.
func SafeLink(u *url.URL) bool {
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	if p := u.Port(); p != "" && (u.Scheme != "http" || p != "80") && (u.Scheme != "https" || p != "443") {
		return false
	}
	_, err := normalizeHost(u.Hostname())
	return err == nil
}

// Normalize validates raw and returns its normalized form.
// An input without a scheme ("example.de/foo") is treated as https.
func Normalize(raw string) (Result, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Result{}, invalid("empty")
	}
	if len(raw) > maxInputLen {
		return Result{}, invalid("longer than %d bytes", maxInputLen)
	}
	if !reScheme.MatchString(raw) {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Result{}, invalid("unparsable")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Result{}, invalid("scheme %q not allowed", u.Scheme)
	}
	if u.User != nil {
		return Result{}, invalid("userinfo not allowed")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return Result{}, invalid("fragment not allowed")
	}
	if p := u.Port(); p != "" && (scheme != "http" || p != "80") && (scheme != "https" || p != "443") {
		return Result{}, invalid("non-default port %s", p)
	}

	host, err := normalizeHost(u.Hostname())
	if err != nil {
		return Result{}, err
	}

	canon := url.URL{Scheme: scheme, Host: host, Path: u.EscapedPath(), RawQuery: stripTracking(u.Query())}
	return Result{Host: host, URL: canon.String()}, nil
}

func normalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "" {
		return "", invalid("missing host")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", invalid("IP literals not allowed")
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", invalid("host is not a valid domain name")
	}
	ascii = strings.TrimPrefix(ascii, "www.")

	labels := strings.Split(ascii, ".")
	if len(labels) < 2 {
		return "", invalid("host must be a fully qualified domain name")
	}
	if tld := labels[len(labels)-1]; isNumericLabel(tld) {
		// Catches legacy IPv4 spellings such as 2130706433, 0x7f.1 or 127.1.
		return "", invalid("IP literals not allowed")
	}
	if slices.ContainsFunc(internalSuffixes, func(s string) bool { return strings.HasSuffix("."+ascii, s) }) {
		return "", invalid("internal host names not allowed")
	}
	return ascii, nil
}

// isNumericLabel reports whether l consists only of digits or is a 0x-prefixed hex number.
func isNumericLabel(l string) bool {
	if l == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		l = rest
		return l != "" && strings.Trim(l, "0123456789abcdef") == ""
	}
	return strings.Trim(l, "0123456789") == ""
}

func stripTracking(q url.Values) string {
	for k := range q {
		lk := strings.ToLower(k)
		if slices.Contains(trackingParams, lk) || slices.ContainsFunc(trackingPrefixes, func(p string) bool { return strings.HasPrefix(lk, p) }) {
			delete(q, k)
		}
	}
	return q.Encode()
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidURL, fmt.Sprintf(format, args...))
}
