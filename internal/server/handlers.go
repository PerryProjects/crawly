package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/brand"
	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/imprint"
	"github.com/OWNER/siteinfo/internal/observability"
	"github.com/OWNER/siteinfo/internal/urlnorm"
)

// Response headers.
const (
	headerCache        = "X-Cache"         // HIT or MISS
	headerCacheRefresh = "X-Cache-Refresh" // "cooldown" when refresh=true was throttled
)

// imprintResponse is the flat JSON body of GET /v1/imprint.
type imprintResponse struct {
	*imprint.Imprint
	Host      string    `json:"host"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
}

// brandResponse is the flat JSON body of GET /v1/brand.
type brandResponse struct {
	*brand.Brand
	Host      string    `json:"host"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
}

func (s *Server) getImprint(c *echo.Context) error {
	return serve(s, c, cache.KindImprint, notFoundError{codeNoImprint, "no imprint page was found for this website"},
		s.imprints.Lookup,
		func(v *imprint.Imprint, host string, r cache.Result[*imprint.Imprint]) any {
			return imprintResponse{Imprint: v, Host: host, FetchedAt: r.FetchedAt, Cached: r.Cached}
		})
}

func (s *Server) getBrand(c *echo.Context) error {
	return serve(s, c, cache.KindBrand, notFoundError{codeNotFound, "no brand data was found for this website"},
		s.brands.Lookup,
		func(v *brand.Brand, host string, r cache.Result[*brand.Brand]) any {
			return brandResponse{Brand: v, Host: host, FetchedAt: r.FetchedAt, Cached: r.Cached}
		})
}

// serve is the shared lookup handler: normalize, read through the cache, render.
func serve[T any](s *Server, c *echo.Context, kind cache.Kind, notFound error,
	lookup func(ctx context.Context, host string) (T, error),
	render func(v T, host string, r cache.Result[T]) any,
) error {
	norm, err := urlnorm.Normalize(c.QueryParam("url"))
	if err != nil {
		return err
	}
	host := norm.Host
	c.Set(ctxHost, host)

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
	ctx = observability.WithLogger(ctx, observability.LoggerFrom(ctx, s.log).With(zap.String("host", host), zap.String("kind", string(kind))))

	res, err := cache.GetOrLoad(ctx, s.loader, cache.Key{Kind: kind, Host: host}, cache.LoadOptions{Refresh: refresh}, func(ctx context.Context) (T, error) {
		return lookup(ctx, host)
	})
	// Negative "error" entries (cached or just loaded) come back with an error but a status.
	if err != nil && res.Status != cache.StatusError {
		return err
	}

	outcome := "MISS"
	if res.Cached {
		outcome = "HIT"
	}
	c.Response().Header().Set(headerCache, outcome)
	c.Set(ctxCache, outcome)
	if err != nil {
		return err
	}
	if res.RefreshThrottled {
		c.Response().Header().Set(headerCacheRefresh, "cooldown")
	}

	if res.Status == cache.StatusNotFound {
		return notFound
	}
	return c.JSON(http.StatusOK, render(res.Data, host, res))
}
