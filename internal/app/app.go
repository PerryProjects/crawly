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
	"go.uber.org/zap"

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
	log   *zap.Logger
	rdb   redis.UniversalClient
	store *cache.Store
	srv   *server.Server

	api       *http.Server
	metrics   *http.Server
	apiLn     net.Listener
	metricsLn net.Listener
}

// New builds all components and binds both listeners (so port conflicts fail fast).
func New(cfg *config.Config, log *zap.Logger) (*App, error) {
	m := observability.NewMetrics()
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
		crawler.WithLogger(log),
		crawler.WithObserver(m),
	)
	imprints := imprint.NewService(cr, imprint.NewFirecrawlExtractor(cr), imprint.WithLogger(log))
	brands := brand.NewService(cr, brand.WithLogger(log))

	store := cache.NewStore(rdb,
		cache.WithLoadTimeout(cfg.Cache.LoadTimeout),
		cache.WithLockMargin(cfg.Cache.LockMargin),
		cache.WithTTLs(cache.TTLs{
			ImprintOK: cfg.Cache.TTL.ImprintOK, BrandOK: cfg.Cache.TTL.BrandOK,
			NotFound: cfg.Cache.TTL.NotFound, Error: cfg.Cache.TTL.Error,
		}),
		cache.WithRefreshCooldown(cfg.Cache.RefreshCool),
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
	}
	if !cfg.Auth.Enabled {
		log.Warn("authentication is DISABLED; use this for local development only")
		srvOpts = append(srvOpts, server.WithoutAuth())
	}
	srv, err := server.New(store, imprints, brands, store, cfg.Auth.Tokens, srvOpts...)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("app: build server: %w", err)
	}

	a := &App{cfg: cfg, log: log, rdb: rdb, store: store, srv: srv}
	a.api = &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout, // longer than the handler deadline
		IdleTimeout:       cfg.Server.IdleTimeout,
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	a.metrics = &http.Server{Handler: mux, ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout}

	if a.apiLn, err = net.Listen("tcp", cfg.Server.Addr); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("app: listen on %s: %w", cfg.Server.Addr, err)
	}
	if a.metricsLn, err = net.Listen("tcp", cfg.Server.MetricsAddr); err != nil {
		_ = a.apiLn.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("app: listen on %s: %w", cfg.Server.MetricsAddr, err)
	}
	return a, nil
}

// APIAddr is the bound address of the API listener.
func (a *App) APIAddr() string { return a.apiLn.Addr().String() }

// MetricsAddr is the bound address of the metrics listener.
func (a *App) MetricsAddr() string { return a.metricsLn.Addr().String() }

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
	case errors.Is(err, crawler.ErrRateLimited):
		return cache.Verdict{}
	case errors.Is(err, crawler.ErrUpstreamTimeout):
		return cache.Verdict{Status: cache.StatusError, Code: crawler.CodeTimeout}
	case errors.Is(err, crawler.ErrUpstream):
		return cache.Verdict{Status: cache.StatusError, Code: crawler.CodeUpstream}
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
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	serve := func(name string, s *http.Server, ln net.Listener) {
		wg.Go(func() {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("app: %s server: %w", name, err)
			}
		})
	}
	serve("api", a.api, a.apiLn)
	serve("metrics", a.metrics, a.metricsLn)
	a.log.Info("serving", zap.String("api", a.APIAddr()), zap.String("metrics", a.MetricsAddr()))

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
	case runErr = <-errCh:
		a.log.Error("server failed, shutting down", zap.Error(runErr))
	}

	shutdownErr := a.shutdown()
	wg.Wait()
	return errors.Join(runErr, shutdownErr)
}

func (a *App) shutdown() error {
	var errs []error

	a.srv.MarkNotReady()
	if d := a.cfg.Server.ShutdownDelay; d > 0 {
		a.log.Info("waiting for endpoint removal", zap.Duration("shutdown_delay", d))
		time.Sleep(d) // deliberately not interruptible: the point is to keep serving while not ready
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	if err := a.api.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("app: api shutdown: %w", err))
		_ = a.api.Close()
	}
	cancel()

	ctx, cancel = context.WithTimeout(context.Background(), a.cfg.Cache.CloseTimeout)
	if err := a.store.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	cancel()

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.metrics.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("app: metrics shutdown: %w", err))
	}
	cancel()

	if err := a.rdb.Close(); err != nil {
		errs = append(errs, fmt.Errorf("app: close redis: %w", err))
	}
	a.log.Info("shutdown complete")
	return errors.Join(errs...)
}
