package imprint

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/perryprojects/siteinfo/internal/urlnorm"
)

// Link is a hyperlink found on a page. Text may be empty.
type Link struct {
	URL  string
	Text string
}

// Candidate is a ranked imprint page candidate.
type Candidate struct {
	URL      string
	Text     string
	Score    int
	External bool // hosted on an unrelated host; surfaced as a warning
	// PathMatch is true when an imprint keyword appears in the URL path (not only in the link text).
	PathMatch bool
}

// keywords identify an imprint link by path or link text. The weight orders the
// terms: "impressum" is the DACH default, "offenlegung" is the Austrian media
// law disclosure which is sometimes a separate page.
var keywords = []struct {
	word   string
	weight int
}{
	{"impressum", 100},
	{"imprint", 90},
	{"anbieterkennung", 70},
	{"legal-notice", 70},
	{"legalnotice", 70},
	{"rechtliche-hinweise", 50},
	{"offenlegung", 40},
}

// privacyWords mark privacy pages, which must never win.
var privacyWords = []string{"datenschutz", "privacy", "privatsphaere", "privatsphäre", "cookie"}

// exactPaths get a large bonus: the canonical imprint locations.
var exactPaths = []string{
	"/impressum", "/imprint", "/legal-notice", "/anbieterkennung",
	"/policies/legal-notice", "/pages/impressum", "/pages/imprint", // Shopify conventions
}

var canonicalizer = strings.NewReplacer("_", "-", " ", "-", " ", "-")

// RankLinks returns imprint candidates, best first.
//
// Rules: a link qualifies if its path or text contains an imprint keyword.
// Pure privacy links are excluded; a combined page ("impressum-datenschutz")
// qualifies with a penalty. Same-host links are preferred, then exact
// "/impressum"-style paths, then shorter paths. Links to unrelated hosts are
// kept (explicitly linked) but flagged External. hosts are the hosts the
// website is known by (the requested host and the host after redirects);
// subdomains and the "www." variant count as related. base resolves relative links.
func RankLinks(hosts []string, base *url.URL, links []Link) []Candidate {
	byKey := map[string]Candidate{}
	for _, l := range links {
		u, ok := resolve(base, l.URL)
		if !ok {
			continue
		}
		c, ok := score(u, l.Text, hosts)
		if !ok {
			continue
		}
		key := dedupKey(u)
		if prev, exists := byKey[key]; !exists || c.Score > prev.Score {
			byKey[key] = c
		}
	}
	out := make([]Candidate, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		if len(a.URL) != len(b.URL) {
			return len(a.URL) - len(b.URL)
		}
		return strings.Compare(a.URL, b.URL)
	})
	return out
}

func resolve(base *url.URL, raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if !urlnorm.SafeLink(u) {
		return nil, false // also SSRF screening: no IP literals, internal names or odd ports
	}
	u.Fragment = ""
	return u, true
}

func dedupKey(u *url.URL) string {
	return hostKey(u.Hostname()) + strings.TrimSuffix(u.EscapedPath(), "/") + "?" + u.RawQuery
}

func hostKey(h string) string { return strings.TrimPrefix(strings.ToLower(h), "www.") }

func related(host string, hosts []string) bool {
	h := hostKey(host)
	return slices.ContainsFunc(hosts, func(known string) bool {
		k := hostKey(known)
		return h == k || strings.HasSuffix(h, "."+k) || strings.HasSuffix(k, "."+h)
	})
}

func score(u *url.URL, text string, hosts []string) (Candidate, bool) {
	path := canonicalizer.Replace(strings.ToLower(pathUnescape(u.Path)))
	txt := canonicalizer.Replace(strings.ToLower(strings.TrimSpace(text)))

	kw, pathMatch := 0, false
	for _, k := range keywords {
		switch {
		case strings.Contains(path, k.word):
			kw, pathMatch = max(kw, k.weight), true
		case strings.Contains(txt, k.word):
			kw = max(kw, k.weight*8/10)
		}
	}
	privacy := slices.ContainsFunc(privacyWords, func(p string) bool { return strings.Contains(path, p) || strings.Contains(txt, p) })
	if kw == 0 {
		return Candidate{}, false // no imprint keyword (this also drops pure privacy links)
	}

	s := kw
	if privacy {
		s -= 30 // combined "impressum-datenschutz" page: usable, but a pure imprint page is better
	}
	if slices.Contains(exactPaths, strings.TrimSuffix(path, "/")) {
		s += 100
	}
	external := !related(u.Hostname(), hosts)
	if external {
		// An unrelated host only wins when the site links nothing of its own: footers also link
		// agencies' or tool vendors' imprints (seen live: kessberlin.de linking askstella.ai).
		s -= 200
	} else {
		s += 50
	}
	if u.RawQuery != "" {
		s -= 20
	}
	s -= min(len(path), 60) + 5*strings.Count(strings.Trim(path, "/"), "/")

	return Candidate{URL: u.String(), Text: strings.TrimSpace(text), Score: s, External: external, PathMatch: pathMatch && !privacy}, true
}

func pathUnescape(p string) string {
	if v, err := url.PathUnescape(p); err == nil {
		return v
	}
	return p
}

var mdLink = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

// MarkdownLinks extracts [text](url) links (not images) from markdown. Firecrawl's
// "links" format returns URLs only, so the markdown is the only source of anchor text.
func MarkdownLinks(md string) []Link {
	var out []Link
	for _, m := range mdLink.FindAllStringSubmatch(md, -1) {
		if m[1] == "!" { // image
			continue
		}
		out = append(out, Link{URL: m[3], Text: m[2]})
	}
	return out
}

// MergeLinks combines Firecrawl's URL list with link text parsed from markdown.
func MergeLinks(urls []string, md string) []Link {
	fromMD := MarkdownLinks(md)
	text := make(map[string]string, len(fromMD))
	for _, l := range fromMD {
		if _, ok := text[l.URL]; !ok {
			text[l.URL] = l.Text
		}
	}
	out := make([]Link, 0, len(urls)+len(fromMD))
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		seen[u] = true
		out = append(out, Link{URL: u, Text: text[u]})
	}
	for _, l := range fromMD {
		if !seen[l.URL] {
			seen[l.URL] = true
			out = append(out, l)
		}
	}
	return out
}
