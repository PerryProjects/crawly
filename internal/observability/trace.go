package observability

import (
	"context"
	"sync"
	"time"
)

// TraceEvent is one step recorded while serving a debug request.
type TraceEvent struct {
	AtMS    float64        `json:"at_ms"` // milliseconds since the trace started
	Stage   string         `json:"stage"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Trace collects a human readable timeline for ONE request, for the debug UI.
// It is opt-in per request; all methods are safe on a nil *Trace, so call sites
// never check whether tracing is on. Only the request that actually runs a load
// carries a trace: callers that share a singleflight result see none.
type Trace struct {
	mu     sync.Mutex
	start  time.Time
	events []TraceEvent
}

const (
	maxTraceEvents = 200
	maxTraceString = 300
)

// NewTrace starts a trace now.
func NewTrace() *Trace { return &Trace{start: time.Now()} }

// Add records an event. kv are alternating string keys and values; strings are truncated
// and durations are rendered in milliseconds.
func (t *Trace) Add(stage, message string, kv ...any) {
	if t == nil {
		return
	}
	var fields map[string]any
	if len(kv) >= 2 {
		fields = make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				continue
			}
			fields[k] = traceValue(kv[i+1])
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) >= maxTraceEvents {
		return
	}
	t.events = append(t.events, TraceEvent{
		AtMS: float64(time.Since(t.start).Microseconds()) / 1000, Stage: stage, Message: message, Fields: fields,
	})
}

// Events returns a copy of the recorded events.
func (t *Trace) Events() []TraceEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceEvent(nil), t.events...)
}

func traceValue(v any) any {
	switch x := v.(type) {
	case string:
		if len(x) > maxTraceString {
			return x[:maxTraceString] + "…"
		}
		return x
	case time.Duration:
		return float64(x.Microseconds()) / 1000
	case error:
		if x == nil {
			return nil
		}
		return traceValue(x.Error())
	default:
		return v
	}
}

type traceKey struct{}

// WithTrace stores t in ctx. The trace survives context.WithoutCancel.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom returns the trace in ctx, or nil (which is safe to use).
func TraceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}
