package observability

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testTraceNilIsSafe(t *testing.T) {
	var tr *Trace
	tr.Add("x", "y", "k", 1)
	if tr.Events() != nil {
		t.Fatal("nil trace has no events")
	}
	if TraceFrom(context.Background()) != nil {
		t.Fatal("no trace in a bare context")
	}
}

func testTraceRecordsAndNormalizesValues(t *testing.T) {
	tr := NewTrace()
	ctx := WithTrace(context.Background(), tr)
	// The trace must survive context.WithoutCancel (detached cache loads).
	TraceFrom(context.WithoutCancel(ctx)).Add("crawler", "call", "url", "https://a.de/", "duration", 1500*time.Microsecond,
		"err", errors.New("boom"), "long", strings.Repeat("x", 1000), "n", 3, 42, "ignored key")
	ev := tr.Events()
	if len(ev) != 1 || ev[0].Stage != "crawler" || ev[0].Message != "call" {
		t.Fatalf("events = %+v", ev)
	}
	f := ev[0].Fields
	if f["duration"] != 1.5 || f["err"] != "boom" || f["n"] != 3 {
		t.Errorf("fields = %v", f)
	}
	if s, _ := f["long"].(string); len(s) > maxTraceString+4 {
		t.Errorf("long value was not truncated: %d bytes", len(s))
	}
	if _, ok := f["42"]; ok || len(f) != 5 {
		t.Errorf("non-string keys must be skipped: %v", f)
	}
	if ev[0].AtMS < 0 {
		t.Error("negative timestamp")
	}
}

func testTraceIsBounded(t *testing.T) {
	tr := NewTrace()
	for range maxTraceEvents + 50 {
		tr.Add("s", "m")
	}
	if n := len(tr.Events()); n != maxTraceEvents {
		t.Fatalf("events = %d, want cap %d", n, maxTraceEvents)
	}
}

func TestTrace(t *testing.T) {
	t.Run("trace nil is safe", testTraceNilIsSafe)
	t.Run("trace records and normalizes values", testTraceRecordsAndNormalizesValues)
	t.Run("trace is bounded", testTraceIsBounded)
}
