package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/perryprojects/siteinfo/internal/brand"
	"github.com/perryprojects/siteinfo/internal/cache"
	"github.com/perryprojects/siteinfo/internal/crawler"
	"github.com/perryprojects/siteinfo/internal/imprint"
	"github.com/perryprojects/siteinfo/internal/observability"
	"github.com/perryprojects/siteinfo/internal/urlnorm"
)

// Response headers.
const (
	headerCache        = "X-Cache"         // HIT or MISS
	headerCacheRefresh = "X-Cache-Refresh" // "cooldown" (refresh throttled) or "kept-previous" (refresh was worse)
)

// imprintResponse is the flat JSON body of GET /v1/imprint.
type imprintResponse struct {
	*imprint.Imprint
	Host      string      `json:"host"`
	FetchedAt time.Time   `json:"fetched_at"`
	Cached    bool        `json:"cached"`
	Debug     *debugBlock `json:"_debug,omitempty"`
}

// brandResponse is the flat JSON body of GET /v1/brand.
type brandResponse struct {
	*brand.Brand
	Host      string      `json:"host"`
	FetchedAt time.Time   `json:"fetched_at"`
	Cached    bool        `json:"cached"`
	Debug     *debugBlock `json:"_debug,omitempty"`
}

func (s *Server) getImprint(c *echo.Context) error {
	return serve(s, c, cache.KindImprint, notFoundError{codeNoImprint, "no imprint page was found for this website"},
		s.imprints.Lookup,
		func(v *imprint.Imprint, host string, r cache.Result[*imprint.Imprint], dbg *debugBlock) any {
			return imprintResponse{Imprint: v, Host: host, FetchedAt: r.FetchedAt, Cached: r.Cached, Debug: dbg}
		})
}

func (s *Server) getBrand(c *echo.Context) error {
	return serve(s, c, cache.KindBrand, notFoundError{codeNotFound, "no brand data was found for this website"},
		s.brands.Lookup,
		func(v *brand.Brand, host string, r cache.Result[*brand.Brand], dbg *debugBlock) any {
			return brandResponse{Brand: v, Host: host, FetchedAt: r.FetchedAt, Cached: r.Cached, Debug: dbg}
		})
}

// serve is the shared lookup handler: normalize, read through the cache, render.
func serve[T any](s *Server, c *echo.Context, kind cache.Kind, notFound error,
	lookup func(ctx context.Context, host string) (T, error),
	render func(v T, host string, r cache.Result[T], dbg *debugBlock) any,
) error {
	dbg := s.newDebugRequest(c, kind)
	norm, err := urlnorm.Normalize(c.QueryParam("url"))
	if err != nil {
		dbg.rejectedURL(c, c.QueryParam("url"), err)
		return err
	}
	host := norm.Host
	dbg.normalized(norm)
	if rf := observability.RequestFieldsFrom(c.Request().Context()); rf != nil {
		rf.Host, rf.Kind = host, string(kind) // read-only from here on; detached loads read them
	}

	refresh := false
	if v := c.QueryParam("refresh"); v != "" && s.refreshEnabled {
		if refresh, err = strconv.ParseBool(v); err != nil {
			return paramError{`refresh must be "true" or "false"`}
		}
	}

	// A context deadline, not a timeout middleware: it bounds the cache wait
	// and flows into the crawler. The detached load runs on its own timeout.
	ctx, cancel := context.WithTimeout(c.Request().Context(), s.handlerTimeout)
	defer cancel()
	ctx = dbg.withTrace(ctx)
	if refresh {
		ctx = crawler.WithFresh(ctx) // also bypass Firecrawl's own cache
	}

	key := cache.Key{Kind: kind, Host: host}
	res, err := cache.GetOrLoad(ctx, s.loader, key, cache.LoadOptions{Refresh: refresh}, func(ctx context.Context) (T, error) {
		return lookup(ctx, host)
	})
	// Negative "error" entries (cached or just loaded) come back with an error but a status.
	if err != nil && res.Status != cache.StatusError {
		dbg.finish(ctx, c, s, key, err)
		return err
	}

	outcome := "MISS"
	if res.Cached {
		outcome = "HIT"
	}
	c.Response().Header().Set(headerCache, outcome)
	c.Set(ctxCache, outcome)
	if err != nil {
		dbg.finish(ctx, c, s, key, err)
		return err
	}
	switch {
	case res.RefreshThrottled:
		c.Response().Header().Set(headerCacheRefresh, "cooldown")
	case res.RefreshKept:
		c.Response().Header().Set(headerCacheRefresh, "kept-previous")
	}

	blk := dbg.finish(ctx, c, s, key, nil)
	if res.Status == cache.StatusNotFound {
		return notFound
	}
	return c.JSON(http.StatusOK, render(res.Data, host, res, blk))
}

// purgeCache deletes everything cached about a host (entries and refresh cooldowns). It serves
// erasure requests (GDPR art. 17) and lets callers force a reload after a correction.
func (s *Server) purgeCache(c *echo.Context) error {
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
