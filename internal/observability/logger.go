// Package observability holds logger helpers and OpenTelemetry metrics.
package observability

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Sampling bounds log volume in production: within one second the first Initial entries with
// the same level and message pass, after that only every Thereafter-th. Initial 0 disables it.
type Sampling struct {
	Initial    int
	Thereafter int
}

// NewLogger builds a zap logger on stderr: JSON in production, colored console in dev.
func NewLogger(env, level string, s Sampling) (*zap.Logger, error) {
	return NewLoggerTo(env, level, s, os.Stderr)
}

// NewLoggerTo is NewLogger with an explicit destination (used by tests and benchmarks).
//
// Logging policy: hot paths (the access log) use typed fields and zapcore.ObjectMarshaler
// structs passed with zap.Inline, which encode without reflection and without allocating.
// Everywhere else the Sugared logger and zap.Any are fine; only global loggers are banned
// (forbidigo). Marshalers are also the place to keep personal data out of logs: leave such
// fields out of MarshalLogObject.
func NewLoggerTo(env, level string, s Sampling, w io.Writer) (*zap.Logger, error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("observability: log level %q: %w", level, err)
	}
	var enc zapcore.Encoder
	if env == "dev" {
		cfg := zap.NewDevelopmentEncoderConfig()
		cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		enc = zapcore.NewConsoleEncoder(cfg)
	} else {
		enc = zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	}
	core := zapcore.NewCore(enc, zapcore.AddSync(w), lvl)
	if env != "dev" && s.Initial > 0 {
		// Bounded log volume under floods: e.g. after 100 identical messages in one second only
		// every 50th is let through until the next second starts.
		core = zapcore.NewSamplerWithOptions(core, time.Second, s.Initial, max(s.Thereafter, 1))
	}
	opts := []zap.Option{zap.ErrorOutput(zapcore.AddSync(os.Stderr))}
	if env == "dev" {
		// Capturing the caller allocates on every log call, so it is a dev-only convenience.
		opts = append(opts, zap.AddCaller())
	}
	return zap.New(core, opts...), nil
}

// RequestFields are the request scoped log fields. They live in the request
// context and are attached to log calls with Fields, instead of building a child
// logger (zap.Logger.With) per request, which allocates.
//
// RequestID is set once by the request-id middleware. Host and Kind are set by
// the handler before the cache is called and are read-only afterwards, so
// detached background loads can read them safely.
type RequestFields struct {
	RequestID string
	Host      string
	Kind      string
}

// MarshalLogObject implements zapcore.ObjectMarshaler without allocating.
func (f *RequestFields) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("request_id", f.RequestID)
	if f.Host != "" {
		enc.AddString("host", f.Host)
	}
	if f.Kind != "" {
		enc.AddString("kind", f.Kind)
	}
	return nil
}

type requestFieldsKey struct{}

// WithRequestFields stores f in ctx.
func WithRequestFields(ctx context.Context, f *RequestFields) context.Context {
	return context.WithValue(ctx, requestFieldsKey{}, f)
}

// RequestFieldsFrom returns the request fields in ctx, or nil.
func RequestFieldsFrom(ctx context.Context) *RequestFields {
	f, _ := ctx.Value(requestFieldsKey{}).(*RequestFields)
	return f
}

// Fields returns the request-scoped fields of ctx as one zap field (a no-op
// field when ctx has none). The values survive context.WithoutCancel, so
// detached cache loads keep their request_id.
func Fields(ctx context.Context) zap.Field {
	if f := RequestFieldsFrom(ctx); f != nil {
		return zap.Inline(f)
	}
	return zap.Skip()
}
