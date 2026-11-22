<!-- Manual checks against the live Firecrawl API. Automated tests never call Firecrawl; they use mocks. -->

# Verification against the real Firecrawl service

One manual smoke test was run on 2026-11-22 against the Firecrawl cloud API (a handful of calls, with the service
binary and a local Redis): imprint lookups for `tagesschau.de` (DE), `orf.at` (AT) and `srf.ch` (CH), a brand lookup for
`tagesschau.de`, a raw `branding` scrape, a raw scrape of a missing page and of a non-existent domain, plus the cache
hit, negative cache and the rate limits on the real binary.

**Confirmed live:** the format strings `markdown`, `links`, `branding` and the `json` format object work as assumed;
footer links are found with `onlyMainContent=false` (the imprint link was discovered on all three sites); imprint pages
were extracted for all three countries (one lookup took 5–9 s); the `branding` shape matches the mapper
(`colorScheme`, `colors.*`, `typography.fontFamilies`, `fonts[{family, role}]`, `images.{logo, favicon, ogImage}`,
`personality`, `brandName`); a **404 on the target site is HTTP 200 with `metadata.statusCode: 404`** (handled); a
**non-existent domain is `success:false`, code `SCRAPE_DNS_RESOLUTION_ERROR` on HTTP 200** (the SDK turns it into
`FirecrawlError{StatusCode: 200}` → 502, negative-cached 5 min).

**Findings that changed the code:** logos are often inline `data:image/svg+xml` URIs (small ones are now kept, large or
non-image data URIs are dropped); a homepage with an error status is now an upstream error instead of a lookup on an
error page; `brandName` was added to the brand DTO.

**Still not verified:** retry behavior against real 429/5xx responses (only against a fake server), behavior under
Firecrawl's concurrency/rate limits, and extraction quality beyond these three sites. Observed limits of the LLM
extraction: for `orf.at` it listed subsidiaries under `represented_by` and found no address; for `tagesschau.de` the
e-mail was obfuscated (`webmaster [at] tagesschau.de`), which is now de-obfuscated (`normalized` warning). A later run showed the grounding check at work: for `orf.at` the model returned a VAT ID that does not appear in the imprint text, and it was removed (`unverified_removed`). The raw `text` of
imprint pages includes navigation and footer noise because the full page is requested.

Observed later: a burst of six lookups exhausted the test account's Firecrawl rate limit; the service answered 503
`upstream_rate_limited` without caching anything, and the same lookups succeeded a minute later. With several pods,
the per-pod `firecrawl.max_concurrency` multiplies: size it so that `pods × max_concurrency` fits the Firecrawl plan.

## 70-domain sweep (2026-11-22)

Imprint and brand were requested for 70 DACH sites (D2C brands, large shops, brand portals and corporate sites), each from
an empty cache, then the failing ones again after fixes. Final state: **67/70 imprints and 67/70 brands**; the three
remaining failures are correct answers about the site (`juniperjack.de` has no DNS record → 422 `host_not_found`;
`kern-ux.de` and `frontify.ch` serve expired TLS certificates → 502 `upstream_error`, not retried). Typical latency from
an empty cache: imprint median ≈ 14 s (p90 ≈ 20 s), brand median ≈ 10 s; about 12 Firecrawl credits per site for both.

Bugs the sweep found and that are fixed: English imprints without the word "Impressum" outside links (gittibeauty.com);
imprint content rendered by JavaScript (aboutyou.de, retried with a render wait); an agency's imprint link outranking the
site's own (kessberlin.de); branding scrapes needing ~30 s on large shops (mediamarkt.de, hornbach.de, naturtreu.de);
Firecrawl answering HTTP 500 with `SCRAPE_SSL_ERROR` (was treated as an outage); identifier spellings such as
"HR B 59603", "HRB 114 439", "HRB-Nr. 730623", two VAT IDs in one line, "Firmenbuchnummer: 42354k", a Swiss UID as
register number, a generic "Handelsregister" type; Cloudflare-protected e-mails; phone numbers written as
"(+49) 0711 …"; redirect tokens in the imprint URL (dfg.de).

Known limits seen in the sweep: on `oebb.at` and `bosch.com` the address is not in the page text Firecrawl returns (also
not with a render wait), so it is reported as `missing`; `blackroll.com` lists two protected e-mail addresses, so none is
picked; country mismatches (a Swiss AG with a German VAT ID) are reported, not "fixed".

## Changes after the sweep (credit savings)

Since the sweep, imprint discovery uses one Firecrawl `map` call (1 credit flat) instead of probing up to three
well-known paths (1 credit per 404), and a strong same-host candidate is fetched with markdown and LLM extraction in one
call (5 credits instead of 1 + 5). In the sweep every imprint was found through a homepage link, so the typical lookup
drops from 7 to 6 credits for the imprint, and a not-found lookup from 4 to 3.
