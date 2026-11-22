// Package server wires the Echo HTTP API: routes, middleware, handlers and
// error mapping. It depends on small consumer-side interfaces only.
package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/brand"
	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/imprint"
)

// ImprintLookup loads an imprint from upstream (the cache miss path).
type ImprintLookup interface {
	Lookup(ctx context.Context, host string) (*imprint.Imprint, error)
}

// BrandLookup loads brand data from upstream (the cache miss path).
type BrandLookup interface {
	Lookup(ctx context.Context, host string) (*brand.Brand, error)
}

// Pinger checks the cache backend for the readiness probe.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RequestObserver records HTTP request metrics. It is implemented by *observability.Metrics.
type RequestObserver interface {
	ObserveRequest(route, method string, status int, d time.Duration)
}

type noopRequestObserver struct{}

func (noopRequestObserver) ObserveRequest(string, string, int, time.Duration) {}

// ErrNoTokens is returned by New when auth is enabled but no token is configured.
var ErrNoTokens = errors.New("server: auth is enabled but no tokens are configured")

// Server is the HTTP API.
type Server struct {
	echo      *echo.Echo
	loader    cache.Loader
	imprints  ImprintLookup
	brands    BrandLookup
	ready     Pinger
	tokenSums [][sha256.Size]byte
	noAuth    bool

	log            *zap.Logger
	obs            RequestObserver
	handlerTimeout time.Duration
	retryAfter     time.Duration
	readyTimeout   time.Duration
	refreshEnabled bool

	shuttingDown atomic.Bool
}

// Option configures optional Server settings.
type Option func(*Server)

// WithLogger sets the base logger.
func WithLogger(l *zap.Logger) Option { return func(s *Server) { s.log = l } }

// WithObserver sets the HTTP metrics sink.
func WithObserver(o RequestObserver) Option { return func(s *Server) { s.obs = o } }

// WithHandlerTimeout sets the context deadline of every lookup request (default 45s).
func WithHandlerTimeout(d time.Duration) Option { return func(s *Server) { s.handlerTimeout = d } }

// WithRetryAfter sets the Retry-After value sent with 503 responses (default 5s).
func WithRetryAfter(d time.Duration) Option { return func(s *Server) { s.retryAfter = d } }

// WithRefresh allows the refresh=true query parameter (disabled by default).
func WithRefresh(enabled bool) Option { return func(s *Server) { s.refreshEnabled = enabled } }

// WithoutAuth disables bearer authentication. This is for local development only.
func WithoutAuth() Option { return func(s *Server) { s.noAuth = true } }

// New builds the server. Bearer authentication is enforced unless WithoutAuth
// is passed; tokens must then contain at least one token.
func New(loader cache.Loader, imprints ImprintLookup, brands BrandLookup, ready Pinger, tokens []string, opts ...Option) (*Server, error) {
	s := &Server{
		loader: loader, imprints: imprints, brands: brands, ready: ready,
		log: zap.NewNop(), obs: noopRequestObserver{},
		handlerTimeout: 45 * time.Second, retryAfter: 5 * time.Second, readyTimeout: time.Second,
	}
	for _, o := range opts {
		o(s)
	}
	if !s.noAuth {
		if len(tokens) == 0 {
			return nil, ErrNoTokens
		}
		// Hashing makes the comparison independent of token length.
		for _, t := range tokens {
			s.tokenSums = append(s.tokenSums, sha256.Sum256([]byte(t)))
		}
	}

	s.echo = echo.NewWithConfig(echo.Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)), // we log with zap
		HTTPErrorHandler: s.handleError,
	})
	s.echo.Use(s.requestID, s.accessLog, s.recoverPanic)

	s.echo.GET("/healthz", s.healthz)
	s.echo.GET("/readyz", s.readyz)
	// Auth is attached per route (not via a Group) so a wrong method still yields 405.
	s.echo.GET("/v1/imprint", s.getImprint, s.authenticate)
	s.echo.GET("/v1/brand", s.getBrand, s.authenticate)
	return s, nil
}

// Handler returns the http.Handler to serve.
func (s *Server) Handler() http.Handler { return s.echo }

// MarkNotReady makes /readyz fail from now on. Called first on shutdown so the
// pod is removed from the Service endpoints before connections are drained.
func (s *Server) MarkNotReady() { s.shuttingDown.Store(true) }

func (s *Server) healthz(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) // liveness: no dependencies
}

func (s *Server) readyz(c *echo.Context) error {
	if s.shuttingDown.Load() {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), s.readyTimeout)
	defer cancel()
	if err := s.ready.Ping(ctx); err != nil {
		s.log.Warn("readiness check failed", zap.Error(err))
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "redis_unavailable"})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
