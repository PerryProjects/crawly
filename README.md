# siteinfo

Microservice that takes a website URL and returns

1. the **legal notice ("Impressum")** of the site, and
2. **brand data** (logo, colors, fonts, description, tone of voice).

Scope: DACH websites (Germany, Austria, Switzerland). The upstream is [Firecrawl](https://www.firecrawl.dev)
(official Go SDK), Redis is the only store and is used purely as a **cache plus distributed lock**. Nothing is
persisted durably; the calling service stores results itself and owns customer overrides.

> **The module path `github.com/perryprojects/siteinfo` is a placeholder and must be renamed** (go.mod, all imports,
> Dockerfile/k8s image name): `go mod edit -module github.com/you/siteinfo` and a repo-wide search/replace of
> `github.com/perryprojects/siteinfo`.

## Contents

- [Architecture](#architecture) · [Cache and lock flow](#cache-and-lock-flow) · [Imprint pipeline](#imprint-pipeline) · [Brand pipeline](#brand-pipeline)
- [API](#api) · [Configuration](#configuration) · [Overload and DDoS protection](#overload-and-ddos-protection) · [Operations](#operations-probes-metrics-shutdown-kubernetes)
- [Development and testing](#development-and-testing)
- [Assumptions and limitations](#assumptions-and-limitations) · [Verification](#verification-against-the-real-firecrawl-service) · [How to extend](#how-to-extend)

## Architecture

```
            GET /v1/imprint?url=   GET /v1/brand?url=
                      │
              ┌───────▼────────┐  request id, panic recovery, access log + OTel metrics,
              │ (/v1) per-IP limit, in-flight cap, bearer auth, per-token limit
              │ internal/server│  (Echo v5) URL normalization -> host, handler deadline, error mapping
              └───────┬────────┘
                      │ cache.GetOrLoad[T]            consumer-side interfaces everywhere
              ┌───────▼────────┐        ┌───────────────────────────┐
              │ internal/cache │◄──────►│ Redis (Cluster or single) │  envelopes + redsync lock + cooldown
              │ singleflight + │        └───────────────────────────┘
              │ redsync loader │
              └───────┬────────┘  (only on a miss)
          ┌───────────┴───────────┐
   ┌──────▼───────┐        ┌──────▼──────┐
   │internal/     │        │internal/    │   discovery, extraction, validation / branding mapping
   │imprint       │        │brand        │
   └──────┬───────┘        └──────┬──────┘
          └───────────┬───────────┘
              ┌───────▼────────┐
              │internal/crawler│  semaphore (4), per-call timeout (30s), retry/backoff, typed errors
              └───────┬────────┘
                      │ official Go SDK
                  Firecrawl API
```

| Package | Responsibility |
|---|---|
| `cmd/siteinfo` | cobra commands `serve`, `version` |
| `internal/config` | typed config, defaults, validation (viper lives only here, unmarshalled once) |
| `internal/app` | manual wiring (no DI framework, no globals, no `init()`), lifecycle, graceful shutdown |
| `internal/server` | Echo setup, middleware, handlers, error mapping |
| `internal/urlnorm` | URL validation and normalization |
| `internal/crawler` | Firecrawl wrapper behind the `Crawler` interface |
| `internal/cache` | Redis store, envelope encoding, singleflight + redsync read-through loader |
| `internal/imprint` | page discovery, extraction, validation |
| `internal/brand` | brand extraction and mapping |
| `internal/observability` | zap logger helpers (zero-allocation request fields), OpenTelemetry metrics |

Dependency direction: `server` → `cache`, `imprint`, `brand`, `urlnorm` · `imprint`/`brand` → `crawler` ·
`cache` → `observability`. Interfaces live at the consumer (`server.ImprintLookup`, `imprint.Scraper`,
`crawler.Scraper`, `cache.Observer`, ...). Required dependencies are constructor parameters; the functional
option pattern is used only for optional settings (timeouts, logger, clock, TTLs, metrics sink).

## Cache and lock flow

`cache.GetOrLoad[T]` (a generic free function over the non-generic `cache.Loader` interface) implements:

1. **Redis GET.** Hit → return. (A cached `error` envelope is replayed as `*cache.CachedError`.)
2. **Miss → `singleflight.DoChan(key)`.** Concurrent callers in the pod share one load. Every caller `select`s on
   its *own* `ctx.Done()`, so one caller disconnecting never fails the others.
3. The load runs on `context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)`: the crawl completes and fills
   the cache even if the leader's client is gone.
4. **redsync mutex** `lock:<kind>:v2:{host}`, expiry = `load_timeout + lock_margin`, retries every 50–100 ms
   (jittered) until the load deadline.
5. **Double-check** the cache (another pod may have filled it while we waited), only then call upstream, `SET` with
   TTL, **unlock on a detached context**.
6. Lock not acquired before the deadline → `ErrLockTimeout` → **503 + `Retry-After`**.

Keys: `imprint:v2:{host}`, `brand:v2:{host}`, locks `lock:imprint:v2:{host}`, cooldown `cooldown:imprint:v2:{host}`.
The `{host}` hash tag keeps all keys of a host in one Redis Cluster slot (verified against a real 3-master cluster,
see below). The version segment lets a schema change roll out under a new key space: it is `v2` since warnings became
structured objects (old `v1` entries are simply ignored and expire).

Envelope (JSON): `schema_version`, `status` (`ok` | `not_found` | `error`), `fetched_at`, `data`, `error_code`.
Undecodable or wrong-version entries are deleted and treated as a miss.

**Trade-offs, stated plainly**

- **The lock is a cost optimization, not a correctness guarantee.** redsync runs against one logical Redis
  (a cluster client talks to one master per slot). A failover or an expired lock can let two pods load the same
  host: one extra Firecrawl call and an idempotent `SET`. Nothing here depends on mutual exclusion.
- **Waiting for the lock spends the load budget.** The 60 s load deadline covers lock wait *and* upstream work. If
  another pod holds the lock for 35 s, we usually end up with a double-check hit instead of a crawl; if it holds it
  longer, the caller gets 503 and retries.
- **Negative results are cached** so broken sites do not burn credits: `not_found` 24 h, `error` 5 min. Only
  upstream failures and per-call timeouts are cached as `error`. Rate limiting and account problems (invalid API
  key, no credits left: `upstream_unavailable`, 503), caller cancellation, deadline expiry and Redis problems say
  nothing about the site and are *never* cached.
- **Degraded results are cached briefly.** An imprint whose LLM extraction failed (only the text is usable) or a brand
  without Firecrawl's branding payload is served, but cached for `cache.ttl.partial` (1 h) instead of 30 days, so a
  transient upstream hiccup is not frozen for a month. `refresh=true` also bypasses Firecrawl's own result cache (`maxAge: 0`).
- A forced refresh never replaces a complete entry with something worse: if the refresh yields an error, `not_found` or a
  degraded result while a complete entry exists, the old entry is kept and served (`X-Cache-Refresh: kept-previous`).
- When the pod-local singleflight leader runs a forced refresh, followers that joined share its result.
- If the Redis `SET` fails after a load, the result is still returned (logged + `siteinfo.cache.write.failures`).
- `X-Cache: HIT` also covers "another pod filled it while we waited for the lock" (no upstream call by us).
- Redis down → lookups return 503 and `/readyz` fails; `/healthz` never touches Redis.

**`refresh=true`** (disabled by default, `cache.refresh_enabled`): bypasses the cache *read* but still goes through
singleflight and the lock. A per-key cooldown (`SET NX PX`, default 1 h) is stored in Redis; within the cooldown the
cached value is served with `X-Cache-Refresh: cooldown`. A refresh with nothing cached simply loads. The cooldown is
consumed when the refresh starts, so a failed refresh also starts the cooldown (abuse protection beats retries).

## Imprint pipeline

1. Scrape the homepage (`markdown` + `links`, full page so footer links survive). If the bare domain fails for site
   reasons (no DNS record, error status), `https://www.<host>/` is tried once, because normalization strips `www.`.
2. **Rank candidate links.** Match path or link text against `impressum`, `imprint`, `anbieterkennung`,
   `legal-notice`, `rechtliche-hinweise`, `offenlegung` (AT media law). Pure privacy/`datenschutz`/cookie links are
   excluded; a combined `impressum-datenschutz` page is kept with a penalty. Preference: same host (subdomains and
   `www.` count as related) > exact `/impressum`-style path > shorter path > no query string. Unrelated hosts are
   allowed only when explicitly linked and produce a warning. Firecrawl's `links` format returns URLs only, so anchor
   text is recovered from the markdown.
3. Try at most the top 2 linked candidates, then **at most 3 well-known paths** (`/impressum`, `/imprint`,
   `/legal-notice`). A page counts only if it returned a success status and contains an imprint marker word
   (rejects soft-404 pages; marker words inside link text do not count, since every footer links "Impressum"). Nothing found → status `not_found` (404, negative-cached).
   A timeout/rate limit/cancellation aborts the lookup instead of being mistaken for "not found".
   **Parent domain:** if the host has no imprint and is a subdomain (`eu.store.bambulab.com`), the registrable parent
   (`bambulab.com`, via the public suffix list; `myshopify.com`-style hosters are excluded) is searched through its
   homepage links, and the result carries a warning that the imprint comes from the parent.
4. Read the chosen page in two steps, behind the `imprint.Extractor` interface: first only the markdown (cheap, ~0.2 s)
   to check it really is an imprint; only an accepted page then gets the Firecrawl JSON extraction (nullable schema,
   "never guess, use null" prompt, ~4–8 s and extra credits). The markdown is kept verbatim as `text`. This way a wrong
   candidate (404, soft 404) costs one fast call instead of a full extraction.
5. Fields (all nullable): `company_name`, `legal_form`, `represented_by[]`, `address{street, postal_code, city,
   country}`, `contact{email, phone, fax}`, `register{type, court, number}`, `vat_id`, `supervisory_authority`,
   `responsible_for_content`, `country` (DE/AT/CH). Placeholder answers (`""`, `null`, `n/a`, `-`) become `null`;
   wrongly typed values become `null` per field.
6. **Deterministic validation → `warnings[]`** (never an error; implemented conservatively):

   | Rule | Check |
   |---|---|
   | DE VAT | `^DE\d{9}$` (after removing whitespace, upper-cased). No checksum. |
   | AT VAT | `^ATU\d{8}$` |
   | CH UID | `^CHE-\d{3}\.\d{3}\.\d{3}(MWST\|TVA\|IVA)?$` |
   | VAT vs country | format country must match the imprint country |
   | Register type | DE: `HRA HRB GnR PR VR` (case-normalized), AT: `FN`; type must belong to the country |
   | DE register number | `^\d{1,7}( ?[A-Z]{1,3})?$` (e.g. `12345 B`) |
   | AT Firmenbuch number | `^\d{1,7} ?[a-z]$` (e.g. `123456a`) |
   | Email | exactly one plain address, dotted domain (`net/mail`); bracketed obfuscation (`name [at] firma [dot] de`, `(at)`, `{ät}`) is rewritten to a plain address (`normalized` warning, never a bare "at"); an otherwise invalid value is removed |
   | Postal code | DE 5 digits, AT/CH 4 digits |
   | Country | normalized to DE/AT/CH; anything else is dropped with a warning |
   | Missing core data | warns when `company_name` or the address is absent |
   | **Grounding** | `company_name`, `email`, `vat_id`, register number and postal code must occur in the page text (case, whitespace and punctuation ignored); otherwise a "not found verbatim" warning. **For identifiers (e-mail, phone, fax, VAT ID, register number) the value is removed**, not just flagged: observed live, the LLM composed `info@<shop-domain>` for a page without any e-mail address. Company name and postal code are only flagged |

   Valid VAT IDs are normalized (whitespace removed), known legal forms get their canonical case (`gmbh` → `GmbH`,
   exact matches only), register numbers lose a glued-on type prefix (`HRB 104144` → `104144`). Nothing else is rewritten.
   The stored `text` is capped at 256 KiB (`text_truncated` warning); validation still sees the full page.

   **Warnings are objects** with a stable `code`, the affected `field` and a human `message`, so a caller can
   highlight fields for review: `missing`, `invalid_format`, `invalid_removed`, `unverified`, `unverified_removed`,
   `country_mismatch`, `not_dach`, `normalized`, `extraction_unavailable`, `parent_domain`, `external_host`,
   `text_truncated` (brand: `branding_unavailable`, `extraction_unavailable`).
7. Response adds `imprint_url`, `fetched_at`, `cached`.

> **The LLM extraction may be wrong. The raw `text` (the page markdown) is the legally authoritative part of the
> response; treat the structured fields as a convenience and check `warnings`.**

## Brand pipeline

One homepage scrape with Firecrawl's `branding` format plus a JSON extraction (`description`, `tone_of_voice`,
`language`). The untyped branding map is mapped into our own DTO — no SDK type reaches the HTTP API:
`logo`, `favicon`, `og_image` (http(s) URLs, relative URLs resolved, and `data:image/*` URIs up to 32 KiB because many
sites inline their SVG logo; all other `data:` URIs are dropped. **Render these with `<img>` only**: an inline SVG opened
as a document or in `<object>`/`<iframe>` can run script), `color_scheme`,
`colors{primary, secondary, accent, background, text_primary, text_secondary}` (hex only; `rgb()` and named colors
are dropped, not guessed), `fonts[]`, `typography{font_families, font_sizes, font_weights}`, `description`
(falls back to page metadata), `tone_of_voice` (≤ 5 lowercase keywords) with `tone_source` and `language`
(primary subtag). Firecrawl's "personality" is used **only** as a fallback for `tone_of_voice` and then flagged as
`tone_source: "firecrawl_hint"`; a hint, not a fact.

## API

All `/v1` endpoints need `Authorization: Bearer <token>`. Errors: `{"error": {"code": "...", "message": "..."}}`.

```bash
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/imprint?url=https://www.example.de/kontakt?utm_source=x' -i
```

```http
HTTP/1.1 200 OK
X-Cache: MISS
X-Request-Id: 3f9c1d2a7b6e5f40
```
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
  "text": "# Impressum\n\nBeispiel GmbH ...",
  "warnings": [
    {"code": "normalized", "field": "contact.email", "message": "contact.email was written obfuscated on the page (\"info [at] example.de\") and has been normalized"}
  ],
  "fetched_at": "2026-11-22T17:15:35Z",
  "cached": false
}
```
*(Illustrative values; the shape is what the tests assert.)*

```bash
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/brand?url=example.de'
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/imprint?url=example.de&refresh=true'   # only if cache.refresh_enabled
curl -s localhost:8080/healthz ; curl -s localhost:8080/readyz
```

| Status | Meaning |
|---|---|
| 200 | ok (`X-Cache: HIT\|MISS`, `cached` flag) |
| 401 | missing/invalid bearer token (`WWW-Authenticate: Bearer`) |
| 429 | rate limited: per IP (`rate_limited`), per token (`token_rate_limited`) or blocked after repeated auth failures (`too_many_auth_failures`); `Retry-After` = time until a token is free |
| 404 | no imprint found (`imprint_not_found`, negative-cached, `X-Cache` set) |
| 422 | invalid URL (`invalid_url`) or malformed parameter (`invalid_parameter`) |
| 502 | upstream error for this site (`upstream_error`, cached 5 min): the target failed (DNS, error status) or Firecrawl answered `success:false` for it |
| 503 | overloaded (`overloaded`: in-flight cap or load admission), lock timeout, Redis down, shutting down, upstream rate limit, or Firecrawl refusing our account (`upstream_unavailable`: API key, credits) — with `Retry-After` |
| 504 | upstream or handler timeout (`timeout`) |
| 499 | client closed the request (non-standard, for log/metric clarity only) |

Response headers: `X-Cache`, `X-Cache-Refresh: cooldown` (refresh throttled) or `kept-previous` (refresh was worse, old entry served), `X-Request-Id` (an incoming
`X-Request-Id` is adopted if it is ≤ 64 chars of `[A-Za-z0-9._-]`).

## Configuration

YAML file (optional, `--config` or `$SITEINFO_CONFIG`) and environment variables. **Env wins over the file.**
Env name = `SITEINFO_` + key path joined with `_`; lists are comma separated; durations use Go syntax (`30s`, `720h`).
The file is unmarshalled once into a validated struct at startup (`internal/config`).
[`config.example.yaml`](config.example.yaml) is a local-dev setup.

| Key | Env | Default | Notes |
|---|---|---|---|
| `env` | `SITEINFO_ENV` | `production` | `dev` = console logs, `production` = JSON logs |
| `log.level` | `SITEINFO_LOG_LEVEL` | `info` | zap level |
| `log.sampling_initial` / `log.sampling_thereafter` | `SITEINFO_LOG_SAMPLING_INITIAL` / `…_THEREAFTER` | `100` / `50` | production only: within one second the first 100 entries with the same level and message pass, then every 50th; initial `0` = off |
| `server.addr` | `SITEINFO_SERVER_ADDR` | `:8080` | API |
| `server.read_header_timeout` | `…_READ_HEADER_TIMEOUT` | `5s` | |
| `server.read_timeout` | `…_READ_TIMEOUT` | `10s` | |
| `server.write_timeout` | `…_WRITE_TIMEOUT` | `80s` | must be > `handler_timeout` |
| `server.idle_timeout` | `…_IDLE_TIMEOUT` | `120s` | |
| `server.handler_timeout` | `…_HANDLER_TIMEOUT` | `65s` | context deadline per lookup |
| `server.retry_after` | `…_RETRY_AFTER` | `5s` | `Retry-After` on 503 |
| `server.shutdown_delay` | `…_SHUTDOWN_DELAY` | `5s` | keep serving (not ready) before draining |
| `server.shutdown_timeout` | `…_SHUTDOWN_TIMEOUT` | `70s` | HTTP drain limit |
| `server.max_header_bytes` | `…_MAX_HEADER_BYTES` | `16384` | request header limit |
| `server.max_connections` | `…_MAX_CONNECTIONS` | `10000` | simultaneous TCP connections, `0` = unlimited |
| `server.trusted_proxies` | `SITEINFO_SERVER_TRUSTED_PROXIES` | – | CIDRs of reverse proxies whose `X-Forwarded-For` is trusted; empty = TCP peer address |
| `auth.enabled` | `SITEINFO_AUTH_ENABLED` | `true` | disable explicitly for dev only |
| `auth.tokens` | `SITEINFO_AUTH_TOKENS` | – | ≥ 1 token, ≥ 16 chars each; several allow rotation |
| `redis.mode` | `SITEINFO_REDIS_MODE` | `cluster` | `cluster` or `single` |
| `redis.addrs` | `SITEINFO_REDIS_ADDRS` | `localhost:6379` | seed nodes (cluster) / first entry (single) |
| `redis.username`, `redis.password` | `SITEINFO_REDIS_USERNAME`, `…_PASSWORD` | – | |
| `redis.db` | `SITEINFO_REDIS_DB` | `0` | single mode only |
| `redis.dial_timeout` / `read_timeout` / `write_timeout` | `SITEINFO_REDIS_…` | `3s` / `2s` / `2s` | |
| `redis.pool_size` | `SITEINFO_REDIS_POOL_SIZE` | `20` | |
| `firecrawl.api_key` | `SITEINFO_FIRECRAWL_API_KEY` | – | required for the cloud API, optional for a self-hosted `api_url` |
| `firecrawl.api_url` | `SITEINFO_FIRECRAWL_API_URL` | `https://api.firecrawl.dev` | |
| `firecrawl.country` / `languages` | `SITEINFO_FIRECRAWL_COUNTRY` / `…_LANGUAGES` | `DE` / `de` | Firecrawl `location`: fetch as a visitor from this country (upper-case ISO code, empty = off) and ask for these languages. Geo-dependent sites otherwise serve the wrong variant, see below |
| `firecrawl.timeout` | `…_TIMEOUT` | `30s` | per call |
| `firecrawl.branding_timeout` | `…_BRANDING_TIMEOUT` | `50s` | per call for the branding scrape, which renders the whole page (measured ~29 s on mediamarkt.de, where a plain scrape takes ~1 s) |
| `firecrawl.max_concurrency` | `…_MAX_CONCURRENCY` | `4` | per pod |
| `firecrawl.max_retries` | `…_MAX_RETRIES` | `2` | on 429/5xx/transport errors |
| `firecrawl.backoff_base` | `…_BACKOFF_BASE` | `500ms` | exponential, equal jitter |
| `cache.load_timeout` | `SITEINFO_CACHE_LOAD_TIMEOUT` | `60s` | lock wait + upstream |
| `cache.lock_margin` | `…_LOCK_MARGIN` | `10s` | lock expiry = load timeout + margin (70 s) |
| `cache.close_timeout` | `…_CLOSE_TIMEOUT` | `65s` | wait for in-flight loads on shutdown |
| `cache.ttl.imprint_ok` / `brand_ok` | `SITEINFO_CACHE_TTL_IMPRINT_OK` / `…_BRAND_OK` | `720h` | 30 d |
| `cache.ttl.not_found` | `SITEINFO_CACHE_TTL_NOT_FOUND` | `24h` | |
| `cache.ttl.error` | `SITEINFO_CACHE_TTL_ERROR` | `5m` | |
| `cache.ttl.partial` | `SITEINFO_CACHE_TTL_PARTIAL` | `1h` | degraded ok results (LLM extraction or branding missing) |
| `cache.refresh_enabled` | `SITEINFO_CACHE_REFRESH_ENABLED` | `false` | allow `refresh=true` |
| `cache.refresh_cooldown` | `SITEINFO_CACHE_REFRESH_COOLDOWN` | `1h` | per key |
| `protect.ip_rate` / `ip_burst` | `SITEINFO_PROTECT_IP_RATE` / `…_IP_BURST` | `50` / `100` | requests/s per client IP (IPv6 per /64), `0` = off |
| `protect.token_rate` / `token_burst` | `SITEINFO_PROTECT_TOKEN_RATE` / `…_TOKEN_BURST` | `50` / `100` | requests/s per API token, `0` = off |
| `protect.auth_failures_per_minute` | `SITEINFO_PROTECT_AUTH_FAILURES_PER_MINUTE` | `10` | failed authentications per IP before it is blocked, `0` = off |
| `protect.max_inflight_requests` | `SITEINFO_PROTECT_MAX_INFLIGHT_REQUESTS` | `256` | concurrent `/v1` requests per pod, `0` = off |
| `protect.max_tracked_keys` | `SITEINFO_PROTECT_MAX_TRACKED_KEYS` | `100000` | bound on limiter memory |
| `protect.max_concurrent_loads` | `SITEINFO_PROTECT_MAX_CONCURRENT_LOADS` | `8` | concurrent upstream loads per pod, `0` = off; keep it near 2 × `firecrawl.max_concurrency`, otherwise loads wait for a Firecrawl slot inside their 60 s budget |
| `protect.load_rate` / `load_burst` | `SITEINFO_PROTECT_LOAD_RATE` / `…_LOAD_BURST` | `2` / `10` | upstream loads (cache misses) per second per pod, `0` = off |
| `debug.enabled` | `SITEINFO_DEBUG_ENABLED` | `false` | debug aids (`debug=1` traces, cache flush, config view); **never in production** |
| `otel.exporter` | `SITEINFO_OTEL_EXPORTER` | `otlp` | `otlp`, `stdout` or `none` |
| `otel.endpoint` | `SITEINFO_OTEL_ENDPOINT` | – | OTLP/HTTP base URL (`http://collector:4318`); empty = standard `OTEL_EXPORTER_OTLP_*` env / `localhost:4318` |
| `otel.interval` | `SITEINFO_OTEL_INTERVAL` | `30s` | export interval |
| `otel.service_name` | `SITEINFO_OTEL_SERVICE_NAME` | `siteinfo` | `OTEL_SERVICE_NAME` wins if set |

**Timeout chain** (validated at startup, each stage longer than the one below):
`firecrawl.timeout 30s` and `firecrawl.branding_timeout 50s` < `cache.load_timeout 60s` < `server.handler_timeout 65s`
< lock expiry `70s`; `server.write_timeout 80s` > handler timeout. **Ingress/Traefik timeouts must be longer than the
handler timeout** (≥ 80 s for read/response timeouts, and no idle timeout shorter than the longest request), otherwise the proxy
cuts requests that the service would still answer.

## Overload and DDoS protection

**Volumetric DDoS must be absorbed at the edge** (CDN/WAF, cloud load balancer, ingress rate limits, network
policies). A Go service behind a saturated link cannot defend itself. What the service does itself is defense in
depth, aimed at the two things that actually hurt here: exhausting the pod, and **burning Firecrawl credits**
(cost amplification: every cache miss costs money, so a client walking through random hostnames is the real attack).

| Layer | Mechanism | Rejection |
|---|---|---|
| Connections | `server.max_connections` (accept blocks at the cap), `ReadHeaderTimeout` (slowloris), `MaxHeaderBytes` | – |
| Per client IP | token bucket `protect.ip_rate/ip_burst`, evaluated first, before authentication; IPv6 grouped by /64 | 429 `rate_limited` |
| Auth failures | `protect.auth_failures_per_minute` failed logins per IP; after that, further *failures* from that IP get 429 instead of 401. A valid token is never blocked, so a stranger cannot lock out callers that share an IP (e.g. everyone behind an ingress without `trusted_proxies`). Brute force is prevented by token length (>= 16 chars), not by this limit | 429 `too_many_auth_failures` |
| Pod capacity | `protect.max_inflight_requests` concurrent `/v1` requests; excess is shed immediately, not queued | 503 `overloaded` |
| Per API token | token bucket `protect.token_rate/token_burst` (after authentication) | 429 `token_rate_limited` |
| **Upstream cost** | cache hits are untouched; *misses* pass admission control: `protect.max_concurrent_loads` and `protect.load_rate` per pod. Plus negative caching (`not_found` 24 h, `error` 5 min) and singleflight/lock dedup | 503 `overloaded` |

Details that matter:

- **Client IP.** By default the TCP peer address is used. `X-Forwarded-For` is honored **only** for peers in
  `server.trusted_proxies`; trusting it blindly would let any client reset its own limit by changing a header. Behind
  an ingress controller you **must** set the proxy CIDR, otherwise all clients share the ingress IP and one bucket.
- **Bounded memory.** Limiter state is capped (`protect.max_tracked_keys`), idle buckets are swept, and once the table
  is full unknown clients share one overflow bucket: under a many-source flood new clients degrade to a shared limit
  instead of the pod running out of memory.
- Probes (`/healthz`, `/readyz`) are never limited or shed. All limits are per pod (no shared Redis counters), so the
  effective limit is `rate × replicas`. Rejections are counted in `siteinfo.protect.rejected{reason}`.
- Defaults are sized for "one trusted calling service plus a few clients". A single backend behind NAT that sends
  more than 50 req/s needs higher `protect.ip_rate`/`token_rate`.
- [`deploy/k8s/traefik-middleware.example.yaml`](deploy/k8s/traefik-middleware.example.yaml) sketches the matching
  edge rate limit / in-flight / body limits for Traefik (an example, not validated against a cluster).

## Operations: probes, metrics, shutdown, Kubernetes

- `/healthz` – liveness, no dependencies (a Redis outage must not restart pods).
- `/readyz` – Redis `PING` (1 s timeout); fails while shutting down.
- **Metrics: OpenTelemetry**, pushed over OTLP/HTTP (`otel.exporter=otlp`, `otel.endpoint`; the standard
  `OTEL_EXPORTER_OTLP_*` variables such as headers and TLS are honored). There is **no `/metrics` endpoint and no
  Prometheus client**; if you scrape with Prometheus, let an OpenTelemetry Collector receive OTLP and expose or
  remote-write it. Pending metrics are flushed on shutdown. Instruments: `siteinfo.cache.lookups{kind,outcome}`,
  `siteinfo.singleflight.shared`, `siteinfo.lock.wait` (s), `siteinfo.load.duration` (s), `siteinfo.loads.inflight`,
  `siteinfo.upstream.calls{outcome}`, `siteinfo.cache.write.failures`, `siteinfo.protect.rejected{reason}`,
  `http.server.request.duration` (s; `http.route`, `http.request.method`, `http.response.status_code`, the OTel
  semantic-convention name) and the Go runtime metrics (`go.*`). Export errors are logged as warnings.
  Only metrics are implemented; traces are not.
- **Logging policy.** The hot path (the access log) is zero-allocation: structs are logged by implementing
  `zapcore.ObjectMarshaler` and passing them with `zap.Inline` (`observability.RequestFields`, the access-log record).
  Request-scoped fields (`request_id`, `host`, `kind`) travel in the context and are attached per call instead of
  building a child logger per request. The access log record and its variadic field slice are pooled;
  `TestAccessLogDoesNotAllocate` and `BenchmarkAccessLog` assert and show **0 allocs/op** (~0.6 µs/op on the dev
  machine). **Everything else uses the Sugared logger** (`Infow`/`Warnw`/`Errorw` with key-value pairs; the
  request-scoped fields from `observability.Fields(ctx)` are passed as the first pair); performance does not matter
  there. Only global loggers are banned (`forbidigo`). Honest scope: the zero-alloc claim covers the access-log call; the
  request itself still allocates (context, request-fields struct, OTel attribute sets).
- **Marshalers keep personal data out of logs.** `MarshalLogObject` decides what a struct reveals. `imprint.Imprint`
  implements it and logs only the imprint URL, country, warning count and text length, never name, address, e-mail or
  phone (a test enforces this), so even an accidental `zap.Object("imprint", imp)` leaks nothing.
- **Sampling** (production only, `log.sampling_*`): within one second the first 100 entries with the same level and
  message pass, then only every 50th, until the next second. Production also disables caller capture (it allocates;
  dev keeps it).
- Logs never contain the query string, the `Authorization` header or Firecrawl payloads; error details from
  upstream/Redis are logged, not returned to clients.

**Graceful shutdown** (SIGTERM/SIGINT via `signal.NotifyContext`): (1) `/readyz` starts failing, (2) wait
`shutdown_delay` while still serving, (3) `http.Server.Shutdown` drains requests (≤ `shutdown_timeout`),
(4) `store.Close(ctx)` waits for in-flight loads (≤ `cache.close_timeout`), (5) Redis is closed.

Kubernetes ([`deploy/k8s`](deploy/k8s), `kubectl kustomize deploy/k8s` renders cleanly; **not applied to a
cluster**): Deployment with probes, resources, `terminationGracePeriodSeconds: 160` (must exceed preStop +
`shutdown_delay` + drain, i.e. > load timeout + delay), `preStop` sleep (native `sleep` action, Kubernetes ≥ 1.30,
because distroless has no `sleep` binary; on older clusters rely on `shutdown_delay`), Service, HPA, PDB,
ConfigMap, and a Secret *template* (`secret.example.yaml`, not in the kustomization).

## Development and testing

```bash
make build            # static binary in bin/
make race             # go test -race -count=1 ./...
make lint             # golangci-lint v2 (see below)
make check            # build + vet + race + lint
cp .env.example .env && docker compose up --build   # Redis + service + debug UI on http://localhost:8081
```

- **Lint needs golangci-lint v2.** v1.x (e.g. 1.64.8) cannot read Go 1.27 export data and fails with
  `export data version 5 is greater than maximum supported version 2`:
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
- Unit tests: urlnorm (IDN/edge cases), link ranking, validators, envelope encoding, error mapping, config, brand
  mapping, crawler retry/backoff/semaphore.
- Concurrency tests (`internal/cache`): N goroutines over two simulated pods trigger **exactly one** upstream load;
  caller cancellation does not fail other waiters and the cache still fills; double-check after lock; lock failure
  maps to `ErrLockTimeout`; `Close` waits for in-flight loads; panics in a load become errors.
- Protection tests: limiter refill/overflow/sweep/memory bound, IPv6 /64 grouping, `X-Forwarded-For` spoofing with and
  without a trusted proxy, brute-force blocking, per-token limits, in-flight shedding, load admission in the cache.
  Observability tests read OTel instruments with a manual reader and check a real OTLP/HTTP export against a fake
  collector. `TestAccessLogDoesNotAllocate` (skipped under `-race`, which instruments allocations) guards the 0-alloc log path.
- Handler tests use `httptest` with fakes; `internal/app` runs the **full stack** (real wiring and listeners, the
  real Firecrawl SDK against a fake HTTP server, miniredis) including graceful shutdown with a request in flight.
- **miniredis does not support cluster mode.** All tests use a single-node client. A build-tagged integration test
  runs the loader against a *real* Redis Cluster:
  `SITEINFO_TEST_REDIS_CLUSTER=127.0.0.1:7000 go test -tags integration -run Cluster ./internal/cache/`.
  It was run once against a 3-master cluster (hash-tag slot affinity and one-load-per-host both passed).
- Verified library APIs are written down in [`docs/VERIFIED_APIS.md`](docs/VERIFIED_APIS.md).

## Debug UI

`docker compose up --build` starts Redis, the service (debug mode on) and a small Vue page on **http://localhost:8081**
(everything is bound to 127.0.0.1). Put your Firecrawl key in `.env` (`cp .env.example .env`, git-ignored). The menu has
three pages:

- **Start**: what the service does, a URL box with example sites, system status (API/Redis readiness, debug mode, runtime,
  cache TTLs, Firecrawl and protection settings) and the last lookups.
- **Crawler**: enter a site URL and press *Impressum*, *Brand* or *Beide*; the result is shown for reading.
- **Debug**: the same lookups, but shown for debugging.

- **Crawler page, result**: the imprint fields as a table (missing values as `—`), validation warnings, the raw imprint text; for brand
  the logo/favicon/OG image, color swatches, fonts, tone of voice.
- **Debug page, status bar**: HTTP status, `X-Cache`, `Retry-After`, browser and server duration, how old the cached data is, the request
  id (to find the log lines), and for errors the error code with an explanation of what it usually means.
- **Debug page, trace**: the timeline of that request: URL normalization, cache hit/miss, lock, every Firecrawl call (formats, attempt,
  duration, queue wait, outcome, `Retry-After`), the page received (status code, markdown size, links), the ranked imprint
  link candidates with scores, why a candidate was accepted or rejected, validation warnings, what gets stored and for how
  long. A trace only exists for the request that actually ran the load; for a cache hit use *Cache vorher leeren*.
- **Cache**: the Redis state of the key (status, age, TTL, size, whether the lock is held, refresh cooldown).
- **Header / JSON / curl**: raw response headers, the full JSON, and a ready-to-run curl command.
- **Konfiguration** shows the effective config (secrets masked) and runtime facts; the page also polls `/readyz`.

How it works: the page talks to `/api/*` of its own nginx container, which proxies to the service and adds the bearer
token (`API_TOKEN`, shared with `SITEINFO_AUTH_TOKENS`), so the browser never sees it. Vue is vendored
(`web/debug/vendor`, Vue 3.5.13) so no CDN and no build step is needed.

The service side is `debug.enabled`: `?debug=1` on `/v1/imprint` and `/v1/brand` adds a `_debug` object (trace, cache info,
timings) to the response, also on errors; `DELETE /v1/debug/cache?url=…[&kind=imprint|brand]` removes the cache entries and
refresh cooldown of a host; `GET /v1/debug/config` returns the redacted config. They sit behind the normal authentication,
but they reveal internals and delete cache entries, so they are off by default and the compose file is the only place that
turns them on.

## Assumptions and limitations

- **SSRF.** `urlnorm` is purely syntactic: http/https only, no userinfo, default ports only, no fragment, no IP
  literals (including decimal/hex spellings), no `localhost`/`.local`/`.internal`/`.lan`/… suffixes. **It does not
  resolve DNS**, so a public name pointing at a private address is not detected. The same checks apply to links
  discovered on crawled pages (imprint candidates), which come from untrusted HTML; redirects that Firecrawl follows
  are outside this service's control. The fetch happens inside
  Firecrawl, so the exposure is Firecrawl's egress, not this pod's. If you run Firecrawl self-hosted, filter its
  egress (no RFC1918/link-local/metadata ranges). Whether the Firecrawl cloud blocks internal targets is not
  verified here.
- **Input leniency.** A URL without a scheme is treated as `https://`; the path and query of the input are ignored
  (the cache key is the normalized *host*; discovery always starts at `https://<host>/`, even if the input said
  `http://`). A fragment (`#...`) is rejected, not stripped.
- **LLM extraction accuracy.** Fields can be wrong or incomplete; validators and the grounding check only catch
  some errors (no checksum verification, no registry lookup). The raw `text` is authoritative.
- **DACH only by prompt and validators, not by TLD.** `.com` sites of German companies must work, so there is no
  TLD allow-list; non-DACH results get a country warning.
- **Personal data.** Imprints of sole proprietors contain personal data (names, addresses, phone numbers). Storing it
  is intended: the operator confirmed that customers keep working with this data in the calling tool, which persists
  it. Here it lives only in Redis for up to the TTL. Logs deliberately contain the host, never the imprint fields
  (`imprint.Imprint` has a log marshaler that omits them).
- **Imprint budget.** A load runs inside the 60 s load budget. Worst case (subdomain without imprint): homepage, 2 linked
  candidates, 3 probes, then the parent's homepage and 2 candidates; every candidate is one fast markdown call and only the
  accepted page gets the slow extraction. Slow sites may still end as 504 (not cached).
- **Geo-dependent sites.** Firecrawl's default location is the US. Live example: `eu.store.bambulab.com` redirects US
  visitors to `us.store.bambulab.com`, and `bambulab.com` only shows its German footer (with the "Impressum" link) to visitors
  from Germany; before `firecrawl.country=DE` the imprint was not found, now it is (via the parent domain). The DE default is
  right for DACH, but a Swiss or Austrian site that only serves its own country could need a different value (one
  country per deployment; per-request country is not implemented).
- **Outages are not site failures.** Firecrawl 5xx, unreachable Firecrawl, and account problems (401/402, pending
  terms) return 503 `upstream_unavailable` and are never cached; only failures that belong to the site (target error
  status, DNS, `success:false`) are cached as `error`. Firecrawl's own 408 counts as a timeout. A retry is only
  started if the load budget still fits a full call, otherwise the classified error is returned.
- **Cold cache after the v2 key bump.** Deploying the v2 key layout starts with an empty cache. The load admission
  limits (`protect.load_rate`, `max_concurrent_loads`) then shed bursts with 503 `overloaded`; callers should retry with
  `Retry-After`.
- **Rate limits and `Retry-After`.** The SDK does not expose `Retry-After`; a transport wrapper captures it. A
  `Retry-After` longer than the remaining budget is not waited for.
- **TTL rationale.** 30 days: imprints and brand identity change rarely and every miss costs Firecrawl credits;
  24 h for `not_found` so a newly published imprint is found within a day while not re-probing every request;
  5 min for upstream errors: long enough to absorb retry storms against a broken site, short enough to recover
  quickly from transient failures; 1 h for degraded results. Use `refresh=true` (if enabled) to force a reload.
- **Protection limits are per pod and in memory** (no shared counters): a client spreading requests over N pods gets
  N times the budget, and a restarted pod forgets its buckets. A Redis-backed limiter would fix that at the price of a
  Redis round trip on every request; edge limiting is the right place for strict global limits.
- **OTLP export is best effort.** Metrics are batched and pushed every `otel.interval`; a crash loses the last interval,
  and an unreachable collector only produces warning logs.
- **HPA** scales on CPU only as a placeholder; the workload is I/O bound.
- Redis TLS is not configurable yet. The Go toolchain in use was 1.27.2 (module says `go 1.27`).

## Verification against the real Firecrawl service

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

### 70-domain sweep (2026-11-22)

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

## How to extend

- **Async mode.** Handlers depend on `cache.Loader` (`Load(ctx, key, fetch, opts) (Entry, error)`). Implement it
  with: Redis GET → on miss enqueue the key (e.g. a Redis stream/Postgres-backed queue owned by the caller) and
  return an `Entry` with a new `pending` status; map it to `202 Accepted` + `Location` in `server.serve`; a worker
  process calls the same `imprint.Service.Lookup` / `brand.Service.Lookup` and writes the envelope. The envelope
  already has `status`, so adding `pending` is backwards compatible; bump the key version if old readers must not
  see it.
- **Registry verification.** `imprint.Extractor` is the seam. Wrap `FirecrawlExtractor` in a decorator that, after
  extraction, queries the Handelsregister / Unternehmensregister (DE), Firmenbuch (AT) or Zefix (CH) by register
  number and adds confirmation or mismatch warnings (or a `verified` block) to `Extraction`. No change to discovery,
  caching, or the HTTP layer is needed.
- **More kinds** (e.g. a privacy-policy URL): add a `cache.Kind`, a service with a `Lookup`, and a handler built on
  `serve[T]`.
