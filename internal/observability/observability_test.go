package observability

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewLogger(t *testing.T) {
	if _, err := NewLogger("dev", "loud", Sampling{}); err == nil {
		t.Fatal("invalid level must fail")
	}
	var prod, dev bytes.Buffer
	pl, err := NewLoggerTo("production", "info", Sampling{}, &prod)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := NewLoggerTo("dev", "debug", Sampling{}, &dev)
	if err != nil {
		t.Fatal(err)
	}
	pl.Debug("hidden")
	pl.Info("shown")
	dl.Debug("dev debug")
	if strings.Contains(prod.String(), "hidden") || !strings.Contains(prod.String(), `"msg":"shown"`) {
		t.Errorf("production log = %q", prod.String())
	}
	if strings.Contains(prod.String(), `"caller"`) {
		t.Error("production logs must not capture the caller (it allocates per call)")
	}
	if !strings.Contains(dev.String(), "dev debug") || !strings.Contains(dev.String(), "observability_test.go") {
		t.Errorf("dev log = %q", dev.String())
	}
}

func TestProductionSampling(t *testing.T) {
	count := func(env string, s Sampling) int {
		var buf bytes.Buffer
		l, err := NewLoggerTo(env, "info", s, &buf)
		if err != nil {
			t.Fatal(err)
		}
		for range 1000 { // identical message, far faster than one second
			l.Info("flood")
		}
		l.Info("different message is sampled separately")
		return strings.Count(buf.String(), "\n")
	}
	// 100 pass, then every 50th: 100 + 900/50 = 118 (+1 other message). A second boundary can
	// reset the window, which only lets more through, so assert bounds instead of an exact count.
	if n := count("production", Sampling{Initial: 100, Thereafter: 50}); n < 101 || n > 250 {
		t.Errorf("sampled production lines = %d, want about 119", n)
	}
	if n := count("production", Sampling{}); n != 1001 {
		t.Errorf("sampling disabled: lines = %d, want 1001", n)
	}
	if n := count("dev", Sampling{Initial: 100, Thereafter: 50}); n != 1001 {
		t.Errorf("dev must never sample: lines = %d", n)
	}
}

func TestRequestFields(t *testing.T) {
	var buf bytes.Buffer
	l, _ := NewLoggerTo("production", "info", Sampling{}, &buf)

	l.Info("no fields", Fields(context.Background()))
	ctx := WithRequestFields(context.Background(), &RequestFields{RequestID: "r1"})
	rf := RequestFieldsFrom(ctx)
	rf.Host, rf.Kind = "acme.de", "imprint"
	// The values must survive context.WithoutCancel (detached cache loads).
	l.Info("with fields", Fields(context.WithoutCancel(ctx)))

	// The same fields work with the Sugared logger (used for everything except the access log).
	l.Sugar().Warnw("sugared", Fields(ctx), "key", "imprint:v2:{acme.de}", "error", io.EOF)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || strings.Contains(lines[0], "request_id") {
		t.Fatalf("lines = %q", lines)
	}
	for _, want := range []string{`"request_id":"r1"`, `"host":"acme.de"`, `"kind":"imprint"`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("%s missing in %s", want, lines[1])
		}
	}
	for _, want := range []string{`"request_id":"r1"`, `"host":"acme.de"`, `"key":"imprint:v2:{acme.de}"`, `"error":"EOF"`} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("sugared line lacks %s: %s", want, lines[2])
		}
	}
	// Flat, not nested: Inline, not Object.
	if strings.Contains(lines[1], `"request":{`) {
		t.Error("fields must be inlined")
	}
}

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func sumValue(t *testing.T, m metricdata.Metrics, kv ...attribute.KeyValue) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want Sum[int64]", m.Name, m.Data)
	}
	want := attribute.NewSet(kv...)
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("%s has no data point for %v", m.Name, kv)
	return 0
}

func histCount(t *testing.T, m metricdata.Metrics, kv ...attribute.KeyValue) uint64 {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want Histogram[float64]", m.Name, m.Data)
	}
	want := attribute.NewSet(kv...)
	for _, dp := range h.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Count
		}
	}
	t.Fatalf("%s has no data point for %v", m.Name, kv)
	return 0
}

func TestMetricsInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := NewMetrics(reader, "siteinfo-test")
	if err != nil {
		t.Fatal(err)
	}
	m.CacheLookup("imprint", "hit")
	m.CacheLookup("imprint", "hit")
	m.SingleflightShared("imprint")
	m.LockWait("imprint", "acquired", 10*time.Millisecond)
	m.LoadFinished("imprint", "ok", time.Second)
	m.InflightLoads(1)
	m.InflightLoads(1)
	m.InflightLoads(-1)
	m.UpstreamCall("ok")
	m.ObserveRequest("/v1/imprint", "GET", 200, time.Millisecond)
	m.CacheWriteFailed()
	m.Rejected("ip_rate")

	got := collect(t, reader)
	if v := sumValue(t, got["siteinfo.cache.lookups"], attribute.String("kind", "imprint"), attribute.String("outcome", "hit")); v != 2 {
		t.Errorf("cache lookups = %d", v)
	}
	if v := sumValue(t, got["siteinfo.singleflight.shared"], attribute.String("kind", "imprint")); v != 1 {
		t.Errorf("shared = %d", v)
	}
	if v := sumValue(t, got["siteinfo.loads.inflight"]); v != 1 {
		t.Errorf("inflight = %d", v)
	}
	if v := sumValue(t, got["siteinfo.upstream.calls"], attribute.String("outcome", "ok")); v != 1 {
		t.Errorf("upstream = %d", v)
	}
	if v := sumValue(t, got["siteinfo.cache.write.failures"]); v != 1 {
		t.Errorf("write failures = %d", v)
	}
	if v := sumValue(t, got["siteinfo.protect.rejected"], attribute.String("reason", "ip_rate")); v != 1 {
		t.Errorf("rejected = %d", v)
	}
	if c := histCount(t, got["siteinfo.lock.wait"], attribute.String("kind", "imprint"), attribute.String("result", "acquired")); c != 1 {
		t.Errorf("lock wait count = %d", c)
	}
	if c := histCount(t, got["siteinfo.load.duration"], attribute.String("kind", "imprint"), attribute.String("outcome", "ok")); c != 1 {
		t.Errorf("load duration count = %d", c)
	}
	httpM := got["http.server.request.duration"]
	if httpM.Unit != "s" {
		t.Errorf("http duration unit = %q", httpM.Unit)
	}
	if c := histCount(t, httpM, attribute.String("http.route", "/v1/imprint"), attribute.String("http.request.method", "GET"), attribute.Int("http.response.status_code", 200)); c != 1 {
		t.Errorf("http count = %d", c)
	}
	var runtimeMetrics int
	for name := range got {
		if strings.HasPrefix(name, "go.") {
			runtimeMetrics++
		}
	}
	if runtimeMetrics == 0 {
		t.Error("Go runtime metrics are missing")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsWithoutExporterIsSafe(t *testing.T) {
	r, err := NewReader(context.Background(), MetricsConfig{Exporter: ExporterNone})
	if err != nil || r != nil {
		t.Fatalf("reader=%v err=%v", r, err)
	}
	m, err := NewMetrics(r, "x")
	if err != nil {
		t.Fatal(err)
	}
	m.CacheLookup("imprint", "hit") // must not panic or block
	m.ObserveRequest("/x", "GET", 200, time.Second)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(context.Background(), MetricsConfig{Exporter: "prometheus"}); err == nil {
		t.Fatal("unknown exporter must fail")
	}
}

// The OTLP/HTTP exporter pushes protobuf to <endpoint>/v1/metrics.
func TestOTLPExport(t *testing.T) {
	var gotPath, gotType atomic.Value
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath.Store(r.URL.Path)
		gotType.Store(r.Header.Get("Content-Type"))
		body.Store(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reader, err := NewReader(context.Background(), MetricsConfig{Exporter: ExporterOTLP, Endpoint: srv.URL, Interval: time.Hour, ServiceName: "siteinfo-test"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMetrics(reader, "siteinfo-test")
	if err != nil {
		t.Fatal(err)
	}
	m.CacheLookup("imprint", "miss")
	if err := m.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath.Load() != "/v1/metrics" || gotType.Load() != "application/x-protobuf" {
		t.Fatalf("path=%v type=%v", gotPath.Load(), gotType.Load())
	}
	b, _ := body.Load().([]byte)
	for _, want := range []string{"siteinfo.cache.lookups", "siteinfo-test", "outcome", "miss"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("payload lacks %q", want)
		}
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
