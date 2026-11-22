//go:build !race

package server

import (
	"context"
	"io"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/perryprojects/siteinfo/internal/observability"
)

// The race detector instruments allocations, so this guard only runs without -race.
// The access log is the hot path: encoding a record must not allocate.
func TestAccessLogDoesNotAllocate(t *testing.T) {
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel)
	s := newTestServer(t, &mockLoader{}, mockLookups{}, WithLogger(zap.New(core)))
	ctx := observability.WithRequestFields(context.Background(), &observability.RequestFields{RequestID: "abc123", Host: "acme.de", Kind: "imprint"})
	e := &accessEntry{Method: "GET", Route: "/v1/imprint", RemoteIP: "203.0.113.7", Cache: "HIT", Status: 200, Bytes: 4321, Duration: 12 * time.Millisecond}

	if allocs := testing.AllocsPerRun(200, func() { s.logAccess(ctx, e) }); allocs != 0 {
		t.Fatalf("logAccess allocates %.0f times per call, want 0", allocs)
	}
}
