package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private registry. It satisfies the small observer interfaces
// declared by the cache, crawler and server packages.
type Metrics struct {
	reg *prometheus.Registry

	cacheLookups   *prometheus.CounterVec
	sfShared       *prometheus.CounterVec
	lockWait       *prometheus.HistogramVec
	loadDuration   *prometheus.HistogramVec
	inflight       prometheus.Gauge
	upstreamCalls  *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	cacheWriteFail prometheus.Counter
}

// NewMetrics registers all collectors on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.cacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "siteinfo_cache_lookups_total",
		Help: "Cache lookups by kind and outcome (hit, miss, hit_after_lock, refresh_throttled).",
	}, []string{"kind", "outcome"})
	m.sfShared = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "siteinfo_singleflight_shared_total",
		Help: "Callers that shared the result of another in-flight load in this pod.",
	}, []string{"kind"})
	m.lockWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "siteinfo_lock_wait_seconds",
		Help:    "Time spent acquiring the distributed lock.",
		Buckets: []float64{.005, .025, .1, .5, 1, 2.5, 5, 10, 20, 40},
	}, []string{"kind", "result"})
	m.loadDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "siteinfo_load_duration_seconds",
		Help:    "Duration of a cache load (lock wait plus upstream work).",
		Buckets: []float64{.1, .5, 1, 2.5, 5, 10, 20, 30, 40, 50},
	}, []string{"kind", "outcome"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "siteinfo_inflight_loads",
		Help: "Cache loads currently running in this pod.",
	})
	m.upstreamCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "siteinfo_upstream_calls_total",
		Help: "Firecrawl calls by outcome (ok, rate_limited, timeout, error).",
	}, []string{"outcome"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "siteinfo_http_request_duration_seconds",
		Help:    "HTTP request duration by route, method and status.",
		Buckets: []float64{.005, .025, .1, .5, 1, 2.5, 5, 10, 20, 40, 60},
	}, []string{"route", "method", "status"})
	m.cacheWriteFail = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "siteinfo_cache_write_failures_total",
		Help: "Loaded results that could not be written to Redis.",
	})
	m.reg.MustRegister(
		m.cacheLookups, m.sfShared, m.lockWait, m.loadDuration, m.inflight,
		m.upstreamCalls, m.httpDuration, m.cacheWriteFail,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// CacheLookup counts a lookup outcome.
func (m *Metrics) CacheLookup(kind, outcome string) {
	m.cacheLookups.WithLabelValues(kind, outcome).Inc()
}

// SingleflightShared counts a caller that piggybacked on another load.
func (m *Metrics) SingleflightShared(kind string) { m.sfShared.WithLabelValues(kind).Inc() }

// LockWait records how long acquiring the lock took.
func (m *Metrics) LockWait(kind, result string, d time.Duration) {
	m.lockWait.WithLabelValues(kind, result).Observe(d.Seconds())
}

// LoadFinished records a load duration.
func (m *Metrics) LoadFinished(kind, outcome string, d time.Duration) {
	m.loadDuration.WithLabelValues(kind, outcome).Observe(d.Seconds())
}

// InflightLoads adjusts the in-flight gauge by delta (+1 / -1).
func (m *Metrics) InflightLoads(delta int) { m.inflight.Add(float64(delta)) }

// CacheWriteFailed counts a failed cache SET.
func (m *Metrics) CacheWriteFailed() { m.cacheWriteFail.Inc() }

// UpstreamCall counts one Firecrawl attempt.
func (m *Metrics) UpstreamCall(outcome string) { m.upstreamCalls.WithLabelValues(outcome).Inc() }

// ObserveRequest records an HTTP request. route must be the route pattern, never the raw path.
func (m *Metrics) ObserveRequest(route, method string, status int, d time.Duration) {
	m.httpDuration.WithLabelValues(route, method, strconv.Itoa(status)).Observe(d.Seconds())
}
