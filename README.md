# siteinfo

A Go microservice that turns a website URL into

- the **legal notice ("Impressum")** of the site: company, representatives, address, contact, register, VAT ID,
  plus the raw page text, and
- **brand data**: logo, favicon, colors, fonts, description, tone of voice.

It is built for DACH websites (Germany, Austria, Switzerland). Pages are fetched through
[Firecrawl](https://www.firecrawl.dev). Redis is used only as a cache and for locking; nothing is stored durably. The
calling service keeps the results in its own database.

**Contents:** [Quick start](#quick-start) · [API](#api) · [How it works](#how-it-works) ·
[Firecrawl credits](#firecrawl-credits) · [Data protection](#data-protection) · [Security](#security) ·
[Configuration](#configuration) · [Operations](#operations) · [Development](#development) ·
[Limitations](#limitations) · [Extending](#extending)

---

## Quick start

```bash
cp .env.example .env          # put your Firecrawl API key into .env (git-ignored)
make up                       # Redis + service + debug UI (docker compose)
```

- Debug UI: <http://localhost:8081> (start page, crawler view, debug view with traces)
- API: <http://localhost:8080> (token from `.env`, default `dev-only-token-change-me-0123456789abcdef`)

```bash
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/imprint?url=fritz-kola.com' | jq
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/brand?url=fritz-kola.com' | jq
```

Without Docker: `make run` (needs a local Redis; auth is off, see [`config.example.yaml`](config.example.yaml)).

---

## API

All `/v1` endpoints require `Authorization: Bearer <token>`.

| Method and path | Purpose |
|---|---|
| `GET /v1/imprint?url=…` | Legal notice of the site |
| `GET /v1/brand?url=…` | Brand data of the site |
| `DELETE /v1/cache?url=…[&kind=imprint\|brand]` | Remove everything cached about a host (erasure requests, corrections) |
| `GET /v1/debug/config` | Effective configuration, secrets masked (debug mode only) |
| `GET /healthz`, `GET /readyz` | Liveness (no dependencies) and readiness (Redis reachable, not shutting down) |

Optional query parameters: `refresh=true` reloads despite the cache (only if `cache.refresh_enabled`, at most once per
`cache.refresh_cooldown`), `debug=1` adds a `_debug` object with a trace of the request (only in debug mode).

The URL may be given with or without scheme; only the host counts (`https://www.Example.de/kontakt?utm_source=x` and
`example.de` are the same entry).

### Imprint response

```json
{
  "host": "example.de",
  "imprint_url": "https://example.de/impressum",
  "company_name": "Beispiel GmbH",
  "legal_form": "GmbH",
  "represented_by": ["Max Mustermann"],
  "address": {"street": "Musterstraße 1", "postal_code": "10115", "city": "Berlin", "country": "DE"},
  "contact": {"email": "info@example.de", "phone": "+49 30 123456", "fax": null},
  "register": {"type": "HRB", "court": "Amtsgericht Berlin (Charlottenburg)", "number": "12345 B"},
  "vat_id": "DE123456789",
  "supervisory_authority": null,
  "responsible_for_content": null,
  "country": "DE",
  "text": "# Impressum\n\nBeispiel GmbH …",
  "warnings": [
    {"code": "normalized", "field": "contact.email", "message": "contact.email was written obfuscated on the page …"}
  ],
  "fetched_at": "2026-11-22T17:15:35Z",
  "cached": false
}
```

- Fields are `null` when the page does not state them; values are never guessed. `other_vat_ids` appears when a page
  lists several VAT IDs.
- **`text` is the raw page content and the legally authoritative part.** The structured fields come from an LLM and
  are checked, but can still be wrong.
- **`warnings`** have a stable `code` and `field` (for highlighting fields for review) and a human `message`:

| Code | Meaning |
|---|---|
| `missing` | an expected field was not found |
| `invalid_format` | value kept, but it does not match the format for its country |
| `invalid_removed` | value was unusable and set to null |
| `unverified` | value kept, but not found verbatim in the page text |
| `unverified_removed` | identifier (e-mail, phone, VAT ID, register number) not on the page, set to null |
| `country_mismatch` | e.g. a German VAT ID on a Swiss imprint |
| `not_dach` | country outside DE/AT/CH |
| `normalized` | rewritten deterministically (`info [at] x.de` → `info@x.de`, `HR B 1234` → `HRB` / `1234`, …) |
| `extraction_unavailable` | the LLM extraction failed; only `text` is usable (cached briefly, see below) |
| `parent_domain` | imprint found on the parent domain (`eu.store.x.com` → `x.com`) |
| `external_host` | the site links an imprint on another host |
| `text_truncated` | page text longer than 256 KiB was cut |

### Brand response

`brand_name`, `logo`, `favicon`, `og_image` (URLs or small inline `data:image/*`), `color_scheme`,
`colors{primary, secondary, accent, background, text_primary, text_secondary}` (hex), `fonts[{family, role}]`,
`typography`, `description`, `tone_of_voice[]` with `tone_source`, `language`, `warnings` (`branding_unavailable`,
`extraction_unavailable`), `host`, `fetched_at`, `cached`.
**Render logos with `<img>` only.** Inline SVGs with scripts or event handlers are dropped, but `<img>` is the safe
way to show third-party images.

### Status codes and headers

| Status | Code(s) | Meaning |
|---|---|---|
| 200 | | ok |
| 401 | `unauthorized` | missing or wrong token |
| 404 | `imprint_not_found` | no imprint page on the site (cached 24 h) |
| 422 | `invalid_url`, `invalid_parameter`, `host_not_found` | bad input; `host_not_found` = the domain has no DNS record |
| 502 | `upstream_error` | the site failed (error status, TLS error, …); cached 5 min |
| 503 | `overloaded`, `unavailable`, `upstream_rate_limited`, `upstream_unavailable` | busy, Redis down, Firecrawl rate limit or outage; never cached; `Retry-After` set |
| 504 | `timeout` | the site or Firecrawl was too slow |

Errors look like `{"error": {"code": "…", "message": "…"}}`. Response headers: `X-Cache: HIT|MISS`,
`X-Cache-Refresh: cooldown|kept-previous`, `X-Request-Id` (also in the logs), `Retry-After` on 503.

---

## How it works

```
request ─► server (auth, overload shedding, URL normalization)
               │
               ▼
           cache (Redis GET ─► singleflight ─► lock ─► double-check ─► load ─► SET)
               │ miss
               ▼
     imprint / brand pipeline ─► crawler (Firecrawl SDK: semaphore, timeouts, retries)
```

**Imprint discovery**

1. Scrape the homepage (markdown and links). If the bare domain fails, `www.<domain>` is tried once.
2. Rank links by path and link text (`impressum`, `imprint`, `anbieterkennung`, `legal-notice`, `offenlegung`, …).
   Privacy links are excluded; the site's own links beat external ones. Discovered links go through the same SSRF
   checks as user input.
3. Try the two best candidates. A strong same-host candidate is scraped together with the LLM extraction in one call.
   Others are first checked cheaply (status, imprint wording outside links), and only then extracted. A linked page that
   looks empty is retried once with a 3 s JavaScript render wait.
4. Nothing found: one Firecrawl `map` search of the site, then a single guess of `/impressum`. For a subdomain, the
   parent domain's homepage links are searched last.
5. Validate deterministically: VAT ID formats (DE/AT/CH), register types and numbers (HRA/HRB/GnR/PR/VR, FN, CH UID),
   postal codes, e-mail syntax (incl. de-obfuscation and Cloudflare-protected addresses), country. Identifiers that do
   not appear in the page text are removed.

**Brand**: one homepage scrape with Firecrawl's `branding` format plus an LLM extraction for description, tone of voice
and language, mapped into the response above.

**Cache and lock**

- Keys `imprint:v2:{host}` / `brand:v2:{host}` hold a JSON envelope (`status` `ok|not_found|error`, `fetched_at`,
  `data`). The `{host}` hash tag keeps all keys of a host in one Redis Cluster slot.
- **TTLs:** 30 days for results, 24 h for "no imprint", 5 min for site failures, 1 h for partial results (LLM
  extraction or branding missing). Firecrawl outages, rate limits and timeouts of the load itself are never cached.
- **One load per host.** Concurrent requests in a pod share one load (singleflight). Across pods a Redis lock
  (redsync) lets one pod load while the others wait and then read the result. The lock lives 15 s and is extended
  while the load runs, so a crashed pod blocks a host for at most 15 s. If Redis fails over mid-load, two pods may load
  the same host once; that costs credits but never produces wrong data.
- A load keeps running and fills the cache even if the client disconnects.
- A forced refresh never replaces a complete entry with a worse result (error, not found, partial); the old entry is
  served with `X-Cache-Refresh: kept-previous`.

---

## Firecrawl credits

Firecrawl charges 1 credit per scraped page (also for 404 pages), +4 for the LLM (`json`) extraction, 1 per `map` call,
and nothing when no page comes back (e.g. DNS failure). Cached results at Firecrawl still cost 1 credit.

| Lookup | Credits |
|---|---|
| Imprint, typical (strong link on the homepage) | 6 = homepage 1 + combined page and extraction 5 |
| Imprint, link found only by its text | 7 = homepage 1 + check 1 + extraction 5 |
| Imprint not found | 3 = homepage 1 + map 1 + one guess 1 |
| Brand | 5 = branding scrape with extraction (observed; branding has no documented surcharge) |
| Cache hit, or repeated "not found" / site failure within its TTL | 0 |

What keeps costs down: 30-day caching and negative caching, one load per host across all pods, the site map instead
of path guessing, combined calls for strong candidates, no extraction for pages that are not an imprint, no retries
that the deadline would cut short, and load admission (`protect.max_concurrent_loads`, `protect.load_rate`) as a hard
ceiling on spending. Size `firecrawl.max_concurrency × instances` to the Firecrawl plan.

---

## Data protection

The service processes personal data: imprints of sole proprietors contain names, addresses and phone numbers. Storing
it is intended, because customers keep working with this data in the calling tool.

- **Storage:** only in Redis, for the TTLs above. `DELETE /v1/cache?url=…` removes a host's data immediately (e.g. for
  erasure requests); the calling service is responsible for its own copy.
- **Logs:** no imprint fields, no request bodies, no query strings, no tokens. Client IPs are shortened (IPv4 /24,
  IPv6 /48). Logs do contain the requested host and a request id.
- **Firecrawl** receives the URLs to fetch and returns public page content. `firecrawl.store_in_cache` is off, so
  Firecrawl does not keep the scraped pages in its result cache. Firecrawl acts as a processor: conclude a data
  processing agreement and check the legal basis for the transfer to its location (or self-host Firecrawl and set
  `firecrawl.api_url`).
- Responses carry `Cache-Control: no-store`. Metrics contain no personal data. Debug mode (traces, config view) is
  off by default and must stay off in production.

---

## Security

- **Authentication:** static bearer tokens of at least 32 characters (e.g. `openssl rand -hex 32`), compared in
  constant time over SHA-256 digests. Several tokens allow rotation. Disabling auth is a dev-only option.
- **SSRF:** user input and every discovered link must be http(s) on a default port with a public-looking host: no IP
  literals (incl. decimal/hex forms), no `localhost`, `.local`, `.internal`, `.lan`, … DNS is not resolved here, so a
  public name pointing to a private address is not detected; the fetch happens inside Firecrawl, whose egress must be
  filtered if you self-host it.
- **Untrusted content:** inline SVG logos with scripts, event handlers or embedded documents are dropped; page text is
  returned as data and never rendered by the service.
- **Hardening:** security headers on every response (`nosniff`, `frame-ancestors 'none'`, no referrer, `no-store`),
  header size and connection limits, read/write/idle timeouts, overload shedding (503) and a ceiling on upstream loads.
  The image is distroless, non-root and contains only the static binary.
- **Supply chain:** `make vuln` runs `govulncheck`; `make update` refreshes dependencies. Secrets come from the
  environment; `.env` is git-ignored and excluded from Docker builds.
- **Not built in:** per-client rate limiting. This is an internal service for trusted callers; put it behind an
  ingress, WAF or API gateway if it is reachable from elsewhere.

---

## Configuration

An optional YAML file (`--config` or `$SITEINFO_CONFIG`) plus environment variables, which win. Environment names are
`SITEINFO_` + the key path with `_` (e.g. `cache.ttl.imprint_ok` → `SITEINFO_CACHE_TTL_IMPRINT_OK`); lists are comma
separated, durations use Go syntax (`30s`, `720h`). The configuration is validated at startup.

| Key | Default | Notes |
|---|---|---|
| `env` | `production` | `dev` = console logs with caller, `production` = JSON logs with sampling |
| `log.level` | `info` | |
| `log.sampling_initial` / `log.sampling_thereafter` | `100` / `50` | production: after 100 identical messages per second, every 50th; `0` = off |
| `server.addr` | `:8080` | |
| `server.handler_timeout` | `65s` | deadline per lookup |
| `server.read_header_timeout` / `read_timeout` / `write_timeout` / `idle_timeout` | `5s` / `10s` / `80s` / `2m` | `write_timeout` must exceed the handler timeout |
| `server.retry_after` | `5s` | `Retry-After` on 503 |
| `server.shutdown_delay` / `shutdown_timeout` | `5s` / `70s` | see [Operations](#operations) |
| `server.max_header_bytes` / `max_connections` | `16384` / `10000` | |
| `auth.enabled` / `auth.tokens` | `true` / – | tokens ≥ 32 characters |
| `redis.mode` / `redis.addrs` | `cluster` / `localhost:6379` | `cluster` or `single`; seed nodes |
| `redis.username` / `password` / `db` / `pool_size` | – / – / `0` / `20` | `db` only in single mode |
| `redis.dial_timeout` / `read_timeout` / `write_timeout` | `3s` / `2s` / `2s` | |
| `firecrawl.api_key` / `api_url` | – / `https://api.firecrawl.dev` | key required for the cloud API |
| `firecrawl.timeout` / `branding_timeout` | `30s` / `50s` | per call; branding renders the whole page |
| `firecrawl.max_concurrency` / `max_retries` / `backoff_base` | `4` / `2` / `500ms` | per instance; retries on 429, 5xx, transport errors |
| `firecrawl.country` / `languages` | `DE` / `de` | Firecrawl location; `.at`/`.ch` sites use AT/CH automatically |
| `firecrawl.store_in_cache` | `false` | let Firecrawl keep scraped pages |
| `cache.load_timeout` / `lock_ttl` / `close_timeout` | `60s` / `15s` / `65s` | |
| `cache.ttl.imprint_ok` / `brand_ok` / `not_found` / `error` / `partial` | `720h` / `720h` / `24h` / `5m` / `1h` | |
| `cache.refresh_enabled` / `refresh_cooldown` | `false` / `1h` | allow `refresh=true` |
| `protect.max_inflight_requests` | `256` | concurrent `/v1` requests per instance; excess gets 503 |
| `protect.max_concurrent_loads` / `load_rate` / `load_burst` | `8` / `2` / `10` | ceiling on upstream loads per instance |
| `otel.exporter` / `endpoint` / `interval` / `service_name` | `otlp` / – / `30s` / `siteinfo` | `otlp`, `stdout` or `none`; standard `OTEL_*` variables apply |
| `debug.enabled` | `false` | traces, config view; never in production |

---

## Operations

- **Probes:** `/healthz` never touches Redis; `/readyz` pings Redis and fails during shutdown.
- **Metrics:** OpenTelemetry, pushed via OTLP/HTTP (no `/metrics` endpoint; use an OpenTelemetry Collector for
  Prometheus). Instruments: `siteinfo.cache.lookups`, `siteinfo.singleflight.shared`, `siteinfo.lock.wait`,
  `siteinfo.load.duration`, `siteinfo.loads.inflight`, `siteinfo.upstream.calls`, `siteinfo.cache.write.failures`,
  `siteinfo.protect.rejected`, `http.server.request.duration`.
- **Logs:** zap, JSON in production. The access log is allocation-free; everything else uses the sugared logger.
  Request id, host and kind are attached to every line of a request.
- **Timeouts:** Firecrawl call 30 s (branding 50 s) < load 60 s < handler 65 s < write 80 s. Proxies in front must
  allow more than 80 s.
- **Shutdown** (SIGTERM/SIGINT): readiness fails, `shutdown_delay` passes while still serving, requests drain
  (≤ `shutdown_timeout`), running loads finish (≤ `cache.close_timeout`), metrics are flushed, Redis is closed. Give the
  orchestrator a grace period of about 160 s.
- **Docker:** `make docker` builds a distroless, non-root, static image; `docker-compose.yml` is the local stack.

---

## Development

```bash
make build   # static binary in bin/
make test    # unit tests
make race    # unit tests with the race detector
make lint    # golangci-lint v2
make vuln    # govulncheck
make check   # build + vet + race + lint + vuln
make update  # newest dependencies
```

- **Tests never call Firecrawl.** They use mocks (`mock*` types) and `httptest` servers; `internal/testguard` makes
  every non-loopback HTTP request fail in the packages that build a Firecrawl client. Tests are grouped as subtests per
  feature (e.g. `go test -run 'TestLookupDiscovery/site_map' ./internal/imprint`).
- Redis tests use miniredis (single node). `SITEINFO_TEST_REDIS_CLUSTER=127.0.0.1:7000 go test -tags integration
  ./internal/cache/` runs the cache against a real Redis Cluster.
- Live checks against Firecrawl are manual; results are in [`docs/VERIFICATION.md`](docs/VERIFICATION.md). Facts about
  third-party APIs are in [`docs/VERIFIED_APIS.md`](docs/VERIFIED_APIS.md).
- Requires Go 1.27 (toolchain `go1.27.4` is pinned in `go.mod`).

---

## Limitations

- **LLM extraction can be wrong.** Validation removes identifiers that are not in the text and flags malformed values,
  but names or addresses can still be off. `text` is authoritative.
- **Pages whose content Firecrawl does not see** (e.g. collapsed sections) yield `missing` warnings; several hidden
  e-mail addresses on one page are not resolved.
- **Imprint text includes navigation and footer**, because the full page is requested so that nothing legal is cut.
- **Only the host counts.** `example.de/brandportal` is looked up as `example.de`; brand data comes from the homepage.
- **Latency on a cache miss:** imprint about 5–20 s, brand about 10 s; large shops up to 30–40 s.
- **Limits are per instance** (overload shedding, load admission, Firecrawl concurrency); there is no shared counter.
- **Redis down means 503** for lookups, so no credits are spent without a cache.

## Extending

- **Asynchronous mode:** handlers depend on the `cache.Loader` interface. A queue-backed loader can return a `pending`
  status (202) and let a worker run the same `Lookup` functions.
- **Registry verification:** `imprint.Extractor` is the seam for checking register numbers against Handelsregister,
  Firmenbuch or Zefix after extraction.
- **More kinds** (e.g. privacy policy URL): add a `cache.Kind`, a service with a `Lookup`, and a handler using
  `serve[T]`.
