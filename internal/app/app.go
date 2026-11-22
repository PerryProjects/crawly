// Package app is the composition root: it wires every dependency by hand in
// New and owns the process lifecycle in Run (start, graceful shutdown).
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"golang.org/x/net/netutil"

	"github.com/OWNER/siteinfo/internal/brand"
	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/config"
	"github.com/OWNER/siteinfo/internal/crawler"
	"github.com/OWNER/siteinfo/internal/imprint"
	"github.com/OWNER/siteinfo/internal/observability"
	"github.com/OWNER/siteinfo/internal/server"
)

// Compile-time proof that both go-redis client types satisfy the interface the code uses.
var (
	_ redis.UniversalClient = (*redis.Client)(nil)
	_ redis.UniversalClient = (*redis.ClusterClient)(nil)
)

// App holds the wired components.
type App struct {
	cfg   *config.Config
	log   *zap.SugaredLogger
	rdb   redis.UniversalClient
	store *cache.Store
	srv   *server.Server

	metrics *observability.Metrics
	api     *http.Server
	apiLn   net.Listener
}

// New builds all components and binds both listeners (so port conflicts fail fast).
func New(cfg *config.Config, log *zap.Logger) (*App, error) {
	reader, err := observability.NewReader(context.Background(), observability.MetricsConfig{
		Exporter: cfg.Otel.Exporter, Endpoint: cfg.Otel.Endpoint, Interval: cfg.Otel.Interval, ServiceName: cfg.Otel.ServiceName,
	})
	if err != nil {
		return nil, err
	}
	sugar := log.Sugar()
	m, err := observability.NewMetrics(reader, cfg.Otel.ServiceName)
	if err != nil {
		return nil, err
	}
	// Export failures (collector down) are reported through OTel's process-wide error handler.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { sugar.Warnw("opentelemetry error", "error", err) }))

	rdb, err := newRedis(cfg.Redis)
	if err != nil {
		return nil, err
	}

	sdk, err := crawler.NewSDK(cfg.Firecrawl.APIKey, cfg.Firecrawl.APIURL)
	if err != nil {
		_ = rdb.Close()
		return nil, err
	}
	cr := crawler.New(sdk,
		crawler.WithTimeout(cfg.Firecrawl.Timeout),
		crawler.WithMaxConcurrency(cfg.Firecrawl.MaxConcurrency),
		crawler.WithMaxRetries(cfg.Firecrawl.MaxRetries),
		crawler.WithBackoff(cfg.Firecrawl.BackoffBase),
		crawler.WithLocation(cfg.Firecrawl.Country, cfg.Firecrawl.Languages),
		crawler.WithLogger(log),
		crawler.WithObserver(m),
	)
	imprints := imprint.NewService(cr, imprint.NewFirecrawlExtractor(cr), imprint.WithLogger(log))
	brands := brand.NewService(cr, brand.WithLogger(log), brand.WithTimeout(cfg.Firecrawl.BrandingTimeout))

	store := cache.NewStore(rdb,
		cache.WithLoadTimeout(cfg.Cache.LoadTimeout),
		cache.WithLockMargin(cfg.Cache.LockMargin),
		cache.WithTTLs(cache.TTLs{
			ImprintOK: cfg.Cache.TTL.ImprintOK, BrandOK: cfg.Cache.TTL.BrandOK,
			NotFound: cfg.Cache.TTL.NotFound, Error: cfg.Cache.TTL.Error, Partial: cfg.Cache.TTL.Partial,
		}),
		cache.WithRefreshCooldown(cfg.Cache.RefreshCool),
		cache.WithLoadLimits(cfg.Protect.MaxConcurrentLoads, cfg.Protect.LoadRate, cfg.Protect.LoadBurst),
		cache.WithClassifier(classify),
		cache.WithLogger(log),
		cache.WithObserver(m),
	)

	srvOpts := []server.Option{
		server.WithLogger(log),
		server.WithObserver(m),
		server.WithHandlerTimeout(cfg.Server.HandlerTimeout),
		server.WithRetryAfter(cfg.Server.RetryAfter),
		server.WithRefresh(cfg.Cache.RefreshEnabled),
		server.WithProtection(server.Protection{
			IPRate: cfg.Protect.IPRate, IPBurst: cfg.Protect.IPBurst,
			TokenRate: cfg.Protect.TokenRate, TokenBurst: cfg.Protect.TokenBurst,
			AuthFailuresPerMinute: cfg.Protect.AuthFailuresPerMinute,
			MaxInflight:           cfg.Protect.MaxInflightRequests,
			MaxTrackedKeys:        cfg.Protect.MaxTrackedKeys,
		}),
	}
	if cfg.Debug.Enabled {
		sugar.Warn("DEBUG mode is enabled: per-request traces, cache flushing and config inspection are available; do not run this in production")
		srvOpts = append(srvOpts, server.WithDebug(cfg.Describe()))
	}
	if len(cfg.Server.TrustedProxies) > 0 {
		nets := make([]*net.IPNet, 0, len(cfg.Server.TrustedProxies))
		for _, cidr := range cfg.Server.TrustedProxies {
			_, n, err := net.ParseCIDR(cidr) // validated by config.Validate
			if err != nil {
				_ = rdb.Close()
				return nil, fmt.Errorf("app: trusted proxy %q: %w", cidr, err)
			}
			nets = append(nets, n)
		}
		srvOpts = append(srvOpts, server.WithTrustedProxies(nets))
	}
	if !cfg.Auth.Enabled {
		sugar.Warn("authentication is DISABLED; use this for local development only")
		srvOpts = append(srvOpts, server.WithoutAuth())
	}
	srv, err := server.New(store, imprints, brands, store, cfg.Auth.Tokens, srvOpts...)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("app: build server: %w", err)
	}

	a := &App{cfg: cfg, log: sugar, rdb: rdb, store: store, srv: srv, metrics: m}
	a.api = &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout, // longer than the handler deadline
		IdleTimeout:       cfg.Server.IdleTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Server.Addr)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("app: listen on %s: %w", cfg.Server.Addr, err)
	}
	if cfg.Server.MaxConnections > 0 {
		ln = netutil.LimitListener(ln, cfg.Server.MaxConnections) // Accept blocks at the cap; excess clients wait in the kernel backlog
	}
	a.apiLn = ln
	return a, nil
}

// APIAddr is the bound address of the API listener.
func (a *App) APIAddr() string { return a.apiLn.Addr().String() }

// newRedis builds a cluster or single-node client. The mode is explicit:
// redis.NewUniversalClient would pick a plain client for a single seed address,
// which is wrong when that address is a Service in front of a cluster.
func newRedis(c config.RedisConfig) (redis.UniversalClient, error) {
	switch c.Mode {
	case "cluster":
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs: c.Addrs, Username: c.Username, Password: c.Password,
			DialTimeout: c.DialTimeout, ReadTimeout: c.ReadTimeout, WriteTimeout: c.WriteTimeout, PoolSize: c.PoolSize,
		}), nil
	case "single":
		return redis.NewClient(&redis.Options{
			Addr: c.Addrs[0], Username: c.Username, Password: c.Password, DB: c.DB,
			DialTimeout: c.DialTimeout, ReadTimeout: c.ReadTimeout, WriteTimeout: c.WriteTimeout, PoolSize: c.PoolSize,
		}), nil
	}
	return nil, fmt.Errorf("app: unknown redis mode %q", c.Mode)
}

// classify decides which load failures are negative-cached.
//
//   - no imprint found: cached as not_found (24h default)
//   - upstream failure or per-call timeout: cached as error (5m default), so a
//     broken site does not burn Firecrawl credits on every request
//   - everything else (rate limiting, caller or deadline cancellation, Redis
//     problems) says nothing about the site and is never cached
func classify(err error) cache.Verdict {
	switch {
	case errors.Is(err, imprint.ErrNotFound):
		return cache.Verdict{Status: cache.StatusNotFound, Code: "not_found"}
	case errors.Is(err, crawler.ErrRateLimited), errors.Is(err, crawler.ErrUpstreamUnavailable):
		return cache.Verdict{} // our account's state, not the site's
	case errors.Is(err, crawler.ErrUpstreamTimeout):
		return cache.Verdict{Status: cache.StatusError, Code: crawler.CodeTimeout}
	case errors.Is(err, crawler.ErrUpstream):
		return cache.Verdict{Status: cache.StatusError, Code: crawler.ErrorCode(err)} // upstream_error or host_not_found
	}
	return cache.Verdict{}
}

// Run serves until ctx is cancelled or a server fails, then shuts down gracefully:
//
//  1. /readyz starts failing (mark not ready)
//  2. wait shutdown_delay so the endpoint is removed from the Service
//  3. http.Server.Shutdown drains in-flight requests (bounded by shutdown_timeout)
//  4. store.Close waits for in-flight loads (bounded by cache.close_timeout)
//  5. the Redis client is closed
func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := a.api.Serve(a.apiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("app: api server: %w", err)
		}
	})
	a.log.Infow("serving", "api", a.APIAddr(), "otel_exporter", a.cfg.Otel.Exporter)

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
	case runErr = <-errCh:
		a.log.Errorw("server failed, shutting down", "error", runErr)
	}

	shutdownErr := a.shutdown(ctx)
	wg.Wait()
	return errors.Join(runErr, shutdownErr)
}

func (a *App) shutdown(parent context.Context) error {
	var errs []error
	// parent is already cancelled (that is why we are here); keep its values, drop its cancellation.
	base := context.WithoutCancel(parent)

	a.srv.MarkNotReady()
	if d := a.cfg.Server.ShutdownDelay; d > 0 {
		a.log.Infow("waiting for endpoint removal", "shutdown_delay", d)
		time.Sleep(d) // deliberately not interruptible: the point is to keep serving while not ready
	}

	ctx, cancel := context.WithTimeout(base, a.cfg.Server.ShutdownTimeout)
	if err := a.api.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("app: api shutdown: %w", err))
		_ = a.api.Close()
	}
	cancel()

	ctx, cancel = context.WithTimeout(base, a.cfg.Cache.CloseTimeout)
	if err := a.store.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	cancel()

	// Flush the last metrics after the drain so the final requests and loads are included.
	ctx, cancel = context.WithTimeout(base, 5*time.Second)
	if err := a.metrics.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	cancel()

	if err := a.rdb.Close(); err != nil {
		errs = append(errs, fmt.Errorf("app: close redis: %w", err))
	}
	a.log.Info("shutdown complete")
	return errors.Join(errs...)
}
