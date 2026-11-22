# Verified third-party APIs

Read from module source (not from memory) on 2026-11-22 with Go 1.27.2.

## Firecrawl Go SDK `github.com/firecrawl/firecrawl/apps/go-sdk` v1.16.9 (go 1.23)
- `firecrawl.NewClient(opts ...option.RequestOption) (*Client, error)`; options: `WithAPIKey`, `WithAPIURL`,
  `WithHTTPClient`, `WithMaxRetries(n)` (default 3), `WithBackoffFactor`, `WithTimeout`, `WithHeader`.
- `(*Client).Scrape(ctx, url string, opts *ScrapeOptions) (*Document, error)` -> POST `/v2/scrape`.
- `ScrapeOptions{Formats []string; JsonOptions *JsonOptions; OnlyMainContent, Timeout(*int, ms), MaxAge(*int64) ...}`.
  `Formats` are plain strings; `JsonOptions{Prompt string; Schema map[string]any}` is serialised as the
  `{"type":"json",...}` format object.
- `Document{Markdown string; JSON any; Metadata map[string]any; Links []string; Branding map[string]any; ...}`.
  **`Links` is `[]string` (URLs only, no anchor text). `Branding` is an untyped map.**
- Errors: `*FirecrawlError{StatusCode, ErrorCode, Message}`, `*AuthenticationError` (401), `*RateLimitError` (429);
  both embed `FirecrawlError`. 429 is **not** retried by the SDK and `Retry-After` is **not** exposed.
  SDK retries 408/409/5xx/transport errors itself (`MaxRetries`); we set it to 0 and retry in `internal/crawler`.
  A `success:false` body on HTTP 200 becomes `*FirecrawlError{StatusCode: 200, ErrorCode: ...}`.
- Branding response shape is **not typed in the SDK**. Field names used in `internal/brand` come from the Firecrawl
  docs cookbook (`colorScheme`, `colors.{primary,secondary,accent,background,textPrimary,textSecondary}`,
  `typography.fontFamilies.{primary,heading,code}`, `typography.fontSizes`, `typography.fontWeights`,
  `images.{logo,favicon,ogImage}`, `logo`). `personality` is only mentioned by third-party sources. The mapper is
  tolerant of missing / differently typed fields. **Unverified against the live API.**

## go-redis `github.com/redis/go-redis/v9` v9.23.0
- `redis.NewClient(*redis.Options) *redis.Client`, `redis.NewClusterClient(*redis.ClusterOptions) *redis.ClusterClient`,
  both satisfy `redis.UniversalClient` (checked by the compiler in `internal/app`).
- `redis.NewUniversalClient` picks the client from `len(Addrs)`; we do not use it because a single Service address in
  front of a cluster would silently select a non-cluster client.

## redsync `github.com/go-redsync/redsync/v4` v4.18.1
- `goredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"`: `goredis.NewPool(redis.UniversalClient) redsyncredis.Pool`.
- `redsync.New(pools ...Pool) *Redsync`; `(*Redsync).NewMutex(name, opts ...Option) *Mutex`.
- Options: `WithExpiry`, `WithTries`, `WithRetryDelay`, `WithRetryDelayFunc(func(tries int) time.Duration)`.
- `(*Mutex).LockContext(ctx) error` returns `redsync.ErrFailed` when it gives up **or when ctx is done**
  (not `ctx.Err()`); a Redis error is only surfaced on the last try. `UnlockContext(ctx) (bool, error)`;
  `ErrLockAlreadyExpired` if the lock expired before unlock.

## Echo `github.com/labstack/echo/v5` v5.4.7
- `type HandlerFunc func(c *echo.Context) error` (Context is a struct pointer), `HTTPErrorHandler func(c *Context, err error)`.
- `echo.NewWithConfig(echo.Config{Logger *slog.Logger, HTTPErrorHandler ...})`; `*Echo` is an `http.Handler`.
- `echo.StatusCode(err)` for 404/405 sentinels (they are no longer `*HTTPError`).
- `Context.Response()` returns `http.ResponseWriter`; `echo.UnwrapResponse(w)` gives `*echo.Response` (Committed, Status).

## Go 1.27
- Generic methods compile (`func (s *S) M[T any]()`), checked with a scratch module. Interfaces still cannot have them,
  and `cache.Loader` is an interface, so `cache.GetOrLoad[T]` is a generic free function.

## OpenTelemetry (go.opentelemetry.io/otel v1.47.0, contrib/instrumentation/runtime v0.72.0)
- `sdkmetric.NewMeterProvider(WithResource, WithReader)`, `sdkmetric.NewPeriodicReader(exp, WithInterval)`,
  `sdkmetric.NewManualReader()` + `Collect` for tests.
- `otlpmetrichttp.New(ctx, opts...)`; `WithEndpoint(host:port)`, `WithURLPath`, `WithInsecure`. **`WithEndpointURL` uses the URL as
  is (like `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`) and would post to `/`**; a base URL like `http://collector:4318` needs
  `WithEndpoint` + `WithURLPath("/v1/metrics")` (checked by `TestOTLPExport`).
- `runtime.Start(runtime.WithMeterProvider(mp))` registers the Go runtime metrics (`go.*`).
- `otel.SetErrorHandler` is the only process-wide setting touched (routes export errors to zap).

## Echo v5 client IP
- Default `Context.RealIP()` is the TCP peer. `Config.IPExtractor = echo.ExtractIPFromXFFHeader(echo.TrustLoopback(false),
  echo.TrustLinkLocal(false), echo.TrustPrivateNet(false), echo.TrustIPRange(cidr)...)` trusts only the listed ranges.

## Observed on the live Firecrawl API (2026-11-22, three DACH sites, see README "Verification")
- Target-site 404: HTTP 200, `data.metadata.statusCode == 404`. Unknown domain: HTTP 200, `success:false`,
  `code: SCRAPE_DNS_RESOLUTION_ERROR`.
- `branding`: keys `colorScheme, fonts[{family,role}], colors{primary,secondary,accent,background,textPrimary,link},
  typography{fontFamilies,fontStacks,fontSizes}, spacing, components, images{logo,favicon,ogImage,logoHref},
  personality{tone,energy,targetAudience}, designSystem, confidence, brandName, logo`. The logo may be a `data:image/svg+xml` URI.
