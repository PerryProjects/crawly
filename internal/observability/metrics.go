package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Exporter kinds for MetricsConfig.Exporter.
const (
	ExporterOTLP   = "otlp"   // OTLP/HTTP push to a collector
	ExporterStdout = "stdout" // pretty JSON on stdout, for local debugging
	ExporterNone   = "none"   // instruments stay cheap no-ops
)

// MetricsConfig selects how metrics leave the process.
type MetricsConfig struct {
	Exporter    string
	Endpoint    string        // OTLP/HTTP base URL; empty uses the standard OTEL_EXPORTER_OTLP_* environment
	Interval    time.Duration // export interval
	ServiceName string
}

// NewReader builds the metric reader for cfg. It returns nil for ExporterNone.
// Standard OTEL_EXPORTER_OTLP_* variables (headers, TLS, compression, timeout)
// are honored by the OTLP exporter itself.
func NewReader(ctx context.Context, cfg MetricsConfig) (sdkmetric.Reader, error) {
	var exp sdkmetric.Exporter
	var err error
	switch cfg.Exporter {
	case ExporterNone, "":
		return nil, nil
	case ExporterOTLP:
		var opts []otlpmetrichttp.Option
		if cfg.Endpoint != "" {
			// The endpoint is a base URL like OTEL_EXPORTER_OTLP_ENDPOINT: the signal path is appended.
			// (otlpmetrichttp.WithEndpointURL would use the URL as is, like the *_METRICS_ENDPOINT variable.)
			u, perr := url.Parse(cfg.Endpoint)
			if perr != nil || u.Host == "" {
				return nil, fmt.Errorf("observability: invalid OTLP endpoint %q", cfg.Endpoint)
			}
			opts = append(opts, otlpmetrichttp.WithEndpoint(u.Host), otlpmetrichttp.WithURLPath(strings.TrimSuffix(u.Path, "/")+"/v1/metrics"))
			if u.Scheme == "http" {
				opts = append(opts, otlpmetrichttp.WithInsecure())
			}
		}
		exp, err = otlpmetrichttp.New(ctx, opts...)
	case ExporterStdout:
		exp, err = stdoutmetric.New()
	default:
		return nil, fmt.Errorf("observability: unknown metrics exporter %q", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("observability: create %s metrics exporter: %w", cfg.Exporter, err)
	}
	return sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(cfg.Interval)), nil
}

// Metrics owns the OpenTelemetry MeterProvider and all instruments. It
// satisfies the small observer interfaces declared by the cache, crawler and
// server packages. Instrument names follow OTel conventions (dots, base units);
// the HTTP histogram uses the semantic-convention name http.server.request.duration.
type Metrics struct {
	mp *sdkmetric.MeterProvider

	cacheLookups   metric.Int64Counter
	sfShared       metric.Int64Counter
	lockWait       metric.Float64Histogram
	loadDuration   metric.Float64Histogram
	inflight       metric.Int64UpDownCounter
	upstreamCalls  metric.Int64Counter
	httpDuration   metric.Float64Histogram
	cacheWriteFail metric.Int64Counter
	rejected       metric.Int64Counter
}

// NewMetrics builds the instruments on a MeterProvider that exports through
// reader (nil reader = no export) and starts Go runtime metrics.
func NewMetrics(reader sdkmetric.Reader, serviceName string) (*Metrics, error) {
	// OTEL_SERVICE_NAME wins over the configured name; OTEL_RESOURCE_ATTRIBUTES are merged in.
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		serviceName = v
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if reader != nil {
		opts = append(opts, sdkmetric.WithReader(reader))
	}
	m := &Metrics{mp: sdkmetric.NewMeterProvider(opts...)}
	meter := m.mp.Meter("github.com/perryprojects/siteinfo")

	seconds := func(buckets ...float64) metric.Float64HistogramOption {
		return metric.WithExplicitBucketBoundaries(buckets...)
	}
	var errs []error
	reg := func(err error) { errs = append(errs, err) }
	var e error
	m.cacheLookups, e = meter.Int64Counter("siteinfo.cache.lookups",
		metric.WithDescription("Cache lookups by kind and outcome (hit, miss, hit_after_lock, refresh_throttled)."), metric.WithUnit("{lookup}"))
	reg(e)
	m.sfShared, e = meter.Int64Counter("siteinfo.singleflight.shared",
		metric.WithDescription("Callers that shared the result of another in-flight load in this pod."), metric.WithUnit("{call}"))
	reg(e)
	m.lockWait, e = meter.Float64Histogram("siteinfo.lock.wait",
		metric.WithDescription("Time spent acquiring the distributed lock."), metric.WithUnit("s"),
		seconds(.005, .025, .1, .5, 1, 2.5, 5, 10, 20, 40))
	reg(e)
	m.loadDuration, e = meter.Float64Histogram("siteinfo.load.duration",
		metric.WithDescription("Duration of a cache load (lock wait plus upstream work)."), metric.WithUnit("s"),
		seconds(.1, .5, 1, 2.5, 5, 10, 20, 30, 40, 50))
	reg(e)
	m.inflight, e = meter.Int64UpDownCounter("siteinfo.loads.inflight",
		metric.WithDescription("Cache loads currently running in this pod."), metric.WithUnit("{load}"))
	reg(e)
	m.upstreamCalls, e = meter.Int64Counter("siteinfo.upstream.calls",
		metric.WithDescription("Firecrawl calls by outcome (ok, rate_limited, timeout, error, canceled)."), metric.WithUnit("{call}"))
	reg(e)
	m.httpDuration, e = meter.Float64Histogram("http.server.request.duration",
		metric.WithDescription("HTTP server request duration."), metric.WithUnit("s"),
		seconds(.005, .025, .1, .5, 1, 2.5, 5, 10, 20, 40, 60))
	reg(e)
	m.cacheWriteFail, e = meter.Int64Counter("siteinfo.cache.write.failures",
		metric.WithDescription("Loaded results that could not be written to Redis."), metric.WithUnit("{write}"))
	reg(e)
	m.rejected, e = meter.Int64Counter("siteinfo.protect.rejected",
		metric.WithDescription("Requests or loads rejected by overload and abuse protection, by reason."), metric.WithUnit("{request}"))
	reg(e)
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("observability: create instruments: %w", err)
	}
	if err := runtime.Start(runtime.WithMeterProvider(m.mp)); err != nil {
		return nil, fmt.Errorf("observability: start runtime metrics: %w", err)
	}
	return m, nil
}

// Shutdown flushes pending metrics and stops the exporter.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if err := m.mp.Shutdown(ctx); err != nil {
		return fmt.Errorf("observability: metrics shutdown: %w", err)
	}
	return nil
}

// ForceFlush exports pending metrics immediately.
func (m *Metrics) ForceFlush(ctx context.Context) error { return m.mp.ForceFlush(ctx) }

// Recording needs no span context, so a background context is correct here.
var bg = context.Background()

func attrs(kv ...attribute.KeyValue) metric.MeasurementOption { return metric.WithAttributes(kv...) }

// CacheLookup counts a lookup outcome.
func (m *Metrics) CacheLookup(kind, outcome string) {
	m.cacheLookups.Add(bg, 1, attrs(attribute.String("kind", kind), attribute.String("outcome", outcome)))
}

// SingleflightShared counts a caller that piggybacked on another load.
func (m *Metrics) SingleflightShared(kind string) {
	m.sfShared.Add(bg, 1, attrs(attribute.String("kind", kind)))
}

// LockWait records how long acquiring the lock took.
func (m *Metrics) LockWait(kind, result string, d time.Duration) {
	m.lockWait.Record(bg, d.Seconds(), attrs(attribute.String("kind", kind), attribute.String("result", result)))
}

// LoadFinished records a load duration.
func (m *Metrics) LoadFinished(kind, outcome string, d time.Duration) {
	m.loadDuration.Record(bg, d.Seconds(), attrs(attribute.String("kind", kind), attribute.String("outcome", outcome)))
}

// InflightLoads adjusts the in-flight gauge by delta (+1 / -1).
func (m *Metrics) InflightLoads(delta int) { m.inflight.Add(bg, int64(delta)) }

// CacheWriteFailed counts a failed cache SET.
func (m *Metrics) CacheWriteFailed() { m.cacheWriteFail.Add(bg, 1) }

// UpstreamCall counts one Firecrawl attempt.
func (m *Metrics) UpstreamCall(outcome string) {
	m.upstreamCalls.Add(bg, 1, attrs(attribute.String("outcome", outcome)))
}

// Rejected counts a request or load turned away by protection (reason is a fixed vocabulary).
func (m *Metrics) Rejected(reason string) {
	m.rejected.Add(bg, 1, attrs(attribute.String("reason", reason)))
}

// ObserveRequest records an HTTP request. route must be the route pattern, never the raw path.
func (m *Metrics) ObserveRequest(route, method string, status int, d time.Duration) {
	m.httpDuration.Record(bg, d.Seconds(), attrs(
		attribute.String("http.route", route),
		attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", status),
	))
}
