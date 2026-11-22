package server

import (
	"context"
	"net/http"
	"os"
	"runtime"
	rtdebug "runtime/debug"
	"slices"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/observability"
	"github.com/OWNER/siteinfo/internal/urlnorm"
)

// ctxDebug is the echo context key holding the *debugBlock for the error handler.
const ctxDebug = "siteinfo.debug"

// cacheDebugger is implemented by *cache.Store; other loaders simply have no cache details.
type cacheDebugger interface {
	Inspect(ctx context.Context, key cache.Key) (cache.Info, error)
	Flush(ctx context.Context, key cache.Key) (int64, error)
}

// debugBlock is the "_debug" object added to responses when the client sends `debug=1`
// and debug mode is enabled. It is for humans; its shape is not a stable API.
type debugBlock struct {
	Input      string                     `json:"input"`
	Host       string                     `json:"host,omitempty"`
	Homepage   string                     `json:"homepage,omitempty"`
	Canonical  string                     `json:"canonical_url,omitempty"`
	Kind       string                     `json:"kind"`
	DurationMS float64                    `json:"duration_ms"`
	Cache      *cache.Info                `json:"cache,omitempty"`
	CacheError string                     `json:"cache_error,omitempty"`
	Trace      []observability.TraceEvent `json:"trace"`
	Note       string                     `json:"note,omitempty"`
}

// debugRequest records one debug request. A nil *debugRequest (debug off or not requested)
// is valid and does nothing, so serve needs no conditionals.
type debugRequest struct {
	trace *observability.Trace
	start time.Time
	blk   debugBlock
}

func (s *Server) newDebugRequest(c *echo.Context, kind cache.Kind) *debugRequest {
	if !s.debug || c.QueryParam("debug") == "" {
		return nil
	}
	return &debugRequest{trace: observability.NewTrace(), start: time.Now(), blk: debugBlock{Kind: string(kind), Input: c.QueryParam("url")}}
}

func (d *debugRequest) withTrace(ctx context.Context) context.Context {
	if d == nil {
		return ctx
	}
	return observability.WithTrace(ctx, d.trace)
}

func (d *debugRequest) normalized(n urlnorm.Result) {
	if d == nil {
		return
	}
	d.blk.Host, d.blk.Homepage, d.blk.Canonical = n.Host, n.Homepage(), n.URL
	d.trace.Add("urlnorm", "input normalized", "input", d.blk.Input, "host", n.Host, "homepage", n.Homepage(), "cache_key_host", n.Host)
}

func (d *debugRequest) rejectedURL(c *echo.Context, input string, err error) {
	if d == nil {
		return
	}
	d.trace.Add("urlnorm", "input rejected", "input", input, "reason", err)
	d.blk.DurationMS = float64(time.Since(d.start).Microseconds()) / 1000
	d.blk.Trace = d.trace.Events()
	c.Set(ctxDebug, &d.blk)
}

// finish builds the debug block, stores it for the error handler and returns it for the success body.
func (d *debugRequest) finish(ctx context.Context, c *echo.Context, s *Server, key cache.Key, err error) *debugBlock {
	if d == nil {
		return nil
	}
	if dc, ok := s.loader.(cacheDebugger); ok {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		info, ierr := dc.Inspect(ictx, key)
		cancel()
		if ierr != nil {
			d.blk.CacheError = ierr.Error()
		} else {
			d.blk.Cache = &info
		}
	}
	d.blk.DurationMS = float64(time.Since(d.start).Microseconds()) / 1000
	d.blk.Trace = d.trace.Events()
	if d.blk.Trace == nil {
		d.blk.Trace = []observability.TraceEvent{}
	}
	if err != nil {
		d.trace.Add("server", "request failed", "error", err)
		d.blk.Trace = d.trace.Events()
	}
	// Followers of a shared load and cache hits run no upstream work of their own.
	if !slices.ContainsFunc(d.blk.Trace, func(e observability.TraceEvent) bool { return e.Stage == "crawler" }) {
		d.blk.Note = "No upstream call was made by this request (cache hit, or it shared another request's load). " +
			"Use \"flush cache\" and request again to see the full load trace."
	}
	c.Set(ctxDebug, &d.blk)
	return &d.blk
}

// flushCache deletes cached entries (and refresh cooldowns) of a host. Debug mode only.
func (s *Server) flushCache(c *echo.Context) error {
	norm, err := urlnorm.Normalize(c.QueryParam("url"))
	if err != nil {
		return err
	}
	dc, ok := s.loader.(cacheDebugger)
	if !ok {
		return notFoundError{codeNotFound, "this cache backend does not support flushing"}
	}
	kinds := []cache.Kind{cache.KindImprint, cache.KindBrand}
	switch k := c.QueryParam("kind"); k {
	case "", "all":
	case string(cache.KindImprint), string(cache.KindBrand):
		kinds = []cache.Kind{cache.Kind(k)}
	default:
		return paramError{`kind must be "imprint", "brand" or "all"`}
	}
	var deleted int64
	keys := make([]string, 0, len(kinds))
	for _, k := range kinds {
		key := cache.Key{Kind: k, Host: norm.Host}
		n, err := dc.Flush(c.Request().Context(), key)
		if err != nil {
			return err
		}
		deleted += n
		keys = append(keys, key.Cache())
	}
	return c.JSON(http.StatusOK, map[string]any{"host": norm.Host, "deleted_keys": deleted, "keys": keys})
}

// showConfig returns the effective (redacted) configuration and runtime facts. Debug mode only.
func (s *Server) showConfig(c *echo.Context) error {
	rt := map[string]any{
		"go_version": runtime.Version(), "goroutines": runtime.NumGoroutine(), "num_cpu": runtime.NumCPU(),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
	}
	if h, err := os.Hostname(); err == nil {
		rt["hostname"] = h
	}
	if bi, ok := rtdebug.ReadBuildInfo(); ok {
		rt["module"], rt["module_version"] = bi.Main.Path, bi.Main.Version
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.revision" {
				rt["vcs_revision"] = kv.Value
			}
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"config": s.debugInfo, "runtime": rt})
}
