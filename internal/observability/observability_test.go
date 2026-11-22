package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewLogger(t *testing.T) {
	for _, env := range []string{"dev", "production"} {
		l, err := NewLogger(env, "debug")
		if err != nil || !l.Core().Enabled(zap.DebugLevel) {
			t.Fatalf("%s: %v", env, err)
		}
	}
	if _, err := NewLogger("dev", "loud"); err == nil {
		t.Fatal("invalid level must fail")
	}
}

func TestLoggerContext(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	fallback := zap.NewNop()
	if LoggerFrom(context.Background(), fallback) != fallback {
		t.Fatal("fallback expected")
	}
	ctx := WithLogger(context.Background(), zap.New(core).With(zap.String("request_id", "r1")))
	// The logger must survive context.WithoutCancel (detached cache loads).
	LoggerFrom(context.WithoutCancel(ctx), fallback).Info("hello")
	if logs.Len() != 1 || logs.All()[0].ContextMap()["request_id"] != "r1" {
		t.Fatalf("logs = %v", logs.All())
	}
}

func TestMetricsExposition(t *testing.T) {
	m := NewMetrics()
	m.CacheLookup("imprint", "hit")
	m.SingleflightShared("imprint")
	m.LockWait("imprint", "acquired", 10*time.Millisecond)
	m.LoadFinished("imprint", "ok", time.Second)
	m.InflightLoads(1)
	m.UpstreamCall("ok")
	m.ObserveRequest("/v1/imprint", "GET", 200, time.Millisecond)
	m.CacheWriteFailed()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body := rec.Body.String()
	for _, want := range []string{
		`siteinfo_cache_lookups_total{kind="imprint",outcome="hit"} 1`,
		`siteinfo_singleflight_shared_total{kind="imprint"} 1`,
		`siteinfo_lock_wait_seconds_count{kind="imprint",result="acquired"} 1`,
		`siteinfo_load_duration_seconds_count{kind="imprint",outcome="ok"} 1`,
		`siteinfo_inflight_loads 1`,
		`siteinfo_upstream_calls_total{outcome="ok"} 1`,
		`siteinfo_http_request_duration_seconds_count{method="GET",route="/v1/imprint",status="200"} 1`,
		`siteinfo_cache_write_failures_total 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
